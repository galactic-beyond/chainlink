package changeset

import (
	"context"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	mapset "github.com/deckarep/golang-set/v2"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/pkg/errors"
	"github.com/smartcontractkit/ccip-owner-contracts/pkg/gethwrappers"
	"github.com/smartcontractkit/chainlink-ccip/pluginconfig"

	commonconfig "github.com/smartcontractkit/chainlink-common/pkg/config"

	commonchangeset "github.com/smartcontractkit/chainlink/deployment/common/changeset"
	commontypes "github.com/smartcontractkit/chainlink/deployment/common/types"

	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"

	"go.uber.org/multierr"
	"go.uber.org/zap/zapcore"

	chainsel "github.com/smartcontractkit/chain-selectors"

	"github.com/smartcontractkit/chainlink-ccip/pkg/reader"
	cciptypes "github.com/smartcontractkit/chainlink-ccip/pkg/types/ccipocr3"
	jobv1 "github.com/smartcontractkit/chainlink-protos/job-distributor/v1/job"
	"github.com/smartcontractkit/chainlink-testing-framework/lib/utils/testcontext"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	commonutils "github.com/smartcontractkit/chainlink-common/pkg/utils"

	"github.com/smartcontractkit/chainlink/deployment"
	"github.com/smartcontractkit/chainlink/deployment/environment/devenv"
	"github.com/smartcontractkit/chainlink/deployment/environment/memory"

	"github.com/smartcontractkit/chainlink/v2/core/gethwrappers/ccip/generated/burn_mint_token_pool"
	"github.com/smartcontractkit/chainlink/v2/core/gethwrappers/ccip/generated/mock_v3_aggregator_contract"
	"github.com/smartcontractkit/chainlink/v2/core/gethwrappers/ccip/generated/onramp"
	"github.com/smartcontractkit/chainlink/v2/core/gethwrappers/ccip/generated/router"
	"github.com/smartcontractkit/chainlink/v2/core/gethwrappers/ccip/generated/usdc_token_pool"
	"github.com/smartcontractkit/chainlink/v2/core/gethwrappers/generated/aggregator_v3_interface"
	"github.com/smartcontractkit/chainlink/v2/core/gethwrappers/generated/mock_ethusd_aggregator_wrapper"
	"github.com/smartcontractkit/chainlink/v2/core/gethwrappers/shared/generated/burn_mint_erc677"
	"github.com/smartcontractkit/chainlink/v2/core/services/ocr2/plugins/ccip/abihelpers"
)

const (
	HomeChainIndex = 0
	FeedChainIndex = 1
)

var (
	// bytes4 public constant EVM_EXTRA_ARGS_V2_TAG = 0x181dcf10;
	evmExtraArgsV2Tag = hexutil.MustDecode("0x181dcf10")

	routerABI = abihelpers.MustParseABI(router.RouterABI)
)

// Context returns a context with the test's deadline, if available.
func Context(tb testing.TB) context.Context {
	ctx := context.Background()
	var cancel func()
	switch t := tb.(type) {
	case *testing.T:
		if d, ok := t.Deadline(); ok {
			ctx, cancel = context.WithDeadline(ctx, d)
		}
	}
	if cancel == nil {
		ctx, cancel = context.WithCancel(ctx)
	}
	tb.Cleanup(cancel)
	return ctx
}

type DeployedEnv struct {
	Env          deployment.Environment
	HomeChainSel uint64
	FeedChainSel uint64
	ReplayBlocks map[uint64]uint64
}

func (e *DeployedEnv) SetupJobs(t *testing.T) {
	ctx := testcontext.Get(t)
	jbs, err := NewCCIPJobSpecs(e.Env.NodeIDs, e.Env.Offchain)
	require.NoError(t, err)
	for nodeID, jobs := range jbs {
		for _, job := range jobs {
			// Note these auto-accept
			_, err := e.Env.Offchain.ProposeJob(ctx,
				&jobv1.ProposeJobRequest{
					NodeId: nodeID,
					Spec:   job,
				})
			require.NoError(t, err)
		}
	}
	// Wait for plugins to register filters?
	// TODO: Investigate how to avoid.
	time.Sleep(30 * time.Second)
	ReplayLogs(t, e.Env.Offchain, e.ReplayBlocks)
}

func ReplayLogs(t *testing.T, oc deployment.OffchainClient, replayBlocks map[uint64]uint64) {
	switch oc := oc.(type) {
	case *memory.JobClient:
		require.NoError(t, oc.ReplayLogs(replayBlocks))
	case *devenv.JobDistributor:
		require.NoError(t, oc.ReplayLogs(replayBlocks))
	default:
		t.Fatalf("unsupported offchain client type %T", oc)
	}
}

func DeployTestContracts(t *testing.T,
	lggr logger.Logger,
	ab deployment.AddressBook,
	homeChainSel,
	feedChainSel uint64,
	chains map[uint64]deployment.Chain,
	linkPrice *big.Int,
	wethPrice *big.Int,
) deployment.CapabilityRegistryConfig {
	capReg, err := DeployCapReg(lggr,
		// deploying cap reg for the first time on a blank chain state
		CCIPOnChainState{
			Chains: make(map[uint64]CCIPChainState),
		}, ab, chains[homeChainSel])
	require.NoError(t, err)
	_, err = DeployFeeds(lggr, ab, chains[feedChainSel], linkPrice, wethPrice)
	require.NoError(t, err)
	evmChainID, err := chainsel.ChainIdFromSelector(homeChainSel)
	require.NoError(t, err)
	return deployment.CapabilityRegistryConfig{
		EVMChainID: evmChainID,
		Contract:   capReg.Address,
	}
}

func LatestBlocksByChain(ctx context.Context, chains map[uint64]deployment.Chain) (map[uint64]uint64, error) {
	latestBlocks := make(map[uint64]uint64)
	for _, chain := range chains {
		latesthdr, err := chain.Client.HeaderByNumber(ctx, nil)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to get latest header for chain %d", chain.Selector)
		}
		block := latesthdr.Number.Uint64()
		latestBlocks[chain.Selector] = block
	}
	return latestBlocks, nil
}

func allocateCCIPChainSelectors(chains map[uint64]deployment.Chain) (homeChainSel uint64, feeChainSel uint64) {
	// Lower chainSel is home chain.
	var chainSels []uint64
	// Say first chain is home chain.
	for chainSel := range chains {
		chainSels = append(chainSels, chainSel)
	}
	sort.Slice(chainSels, func(i, j int) bool {
		return chainSels[i] < chainSels[j]
	})
	// Take lowest for determinism.
	return chainSels[HomeChainIndex], chainSels[FeedChainIndex]
}

// NewMemoryEnvironment creates a new CCIP environment
// with capreg, fee tokens, feeds and nodes set up.
func NewMemoryEnvironment(
	t *testing.T,
	lggr logger.Logger,
	numChains int,
	numNodes int,
	linkPrice *big.Int,
	wethPrice *big.Int) DeployedEnv {
	require.GreaterOrEqual(t, numChains, 2, "numChains must be at least 2 for home and feed chains")
	require.GreaterOrEqual(t, numNodes, 4, "numNodes must be at least 4")
	ctx := testcontext.Get(t)
	chains := memory.NewMemoryChains(t, numChains)
	homeChainSel, feedSel := allocateCCIPChainSelectors(chains)
	replayBlocks, err := LatestBlocksByChain(ctx, chains)
	require.NoError(t, err)

	ab := deployment.NewMemoryAddressBook()
	crConfig := DeployTestContracts(t, lggr, ab, homeChainSel, feedSel, chains, linkPrice, wethPrice)
	nodes := memory.NewNodes(t, zapcore.InfoLevel, chains, numNodes, 1, crConfig)
	for _, node := range nodes {
		require.NoError(t, node.App.Start(ctx))
		t.Cleanup(func() {
			require.NoError(t, node.App.Stop())
		})
	}
	e := memory.NewMemoryEnvironmentFromChainsNodes(t, lggr, chains, nodes)
	envNodes, err := deployment.NodeInfo(e.NodeIDs, e.Offchain)
	require.NoError(t, err)
	e.ExistingAddresses = ab
	_, err = deployHomeChain(lggr, e, e.ExistingAddresses, chains[homeChainSel],
		NewTestRMNStaticConfig(),
		NewTestRMNDynamicConfig(),
		NewTestNodeOperator(chains[homeChainSel].DeployerKey.From),
		map[string][][32]byte{
			"NodeOperator": envNodes.NonBootstraps().PeerIDs(),
		},
	)
	require.NoError(t, err)

	return DeployedEnv{
		Env:          e,
		HomeChainSel: homeChainSel,
		FeedChainSel: feedSel,
		ReplayBlocks: replayBlocks,
	}
}

// NewMemoryEnvironmentWithJobs creates a new CCIP environment
// with capreg, fee tokens, feeds, nodes and jobs set up.
func NewMemoryEnvironmentWithJobs(t *testing.T, lggr logger.Logger, numChains int, numNodes int) DeployedEnv {
	e := NewMemoryEnvironment(t, lggr, numChains, numNodes, MockLinkPrice, MockWethPrice)
	e.SetupJobs(t)
	return e
}

// mockAttestationResponse mocks the USDC attestation server, it returns random Attestation.
// We don't need to return exactly the same attestation, because our Mocked USDC contract doesn't rely on any specific
// value, but instead of that it just checks if the attestation is present. Therefore, it makes the test a bit simpler
// and doesn't require very detailed mocks. Please see tests in chainlink-ccip for detailed tests using real attestations
func mockAttestationResponse() *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := `{
			"status": "complete",
			"attestation": "0x9049623e91719ef2aa63c55f357be2529b0e7122ae552c18aff8db58b4633c4d3920ff03d3a6d1ddf11f06bf64d7fd60d45447ac81f527ba628877dc5ca759651b08ffae25a6d3b1411749765244f0a1c131cbfe04430d687a2e12fd9d2e6dc08e118ad95d94ad832332cf3c4f7a4f3da0baa803b7be024b02db81951c0f0714de1b"
		}`

		_, err := w.Write([]byte(response))
		if err != nil {
			panic(err)
		}
	}))
	return server
}

type TestConfigs struct {
	IsUSDC       bool
	IsMultiCall3 bool
}

func NewMemoryEnvironmentWithJobsAndContracts(t *testing.T, lggr logger.Logger, numChains int, numNodes int, tCfg *TestConfigs) DeployedEnv {
	var err error
	e := NewMemoryEnvironment(t, lggr, numChains, numNodes, MockLinkPrice, MockWethPrice)
	allChains := e.Env.AllChainSelectors()
	cfg := commontypes.MCMSWithTimelockConfig{
		Canceller:         commonchangeset.SingleGroupMCMS(t),
		Bypasser:          commonchangeset.SingleGroupMCMS(t),
		Proposer:          commonchangeset.SingleGroupMCMS(t),
		TimelockExecutors: e.Env.AllDeployerKeys(),
		TimelockMinDelay:  big.NewInt(0),
	}
	mcmsCfg := make(map[uint64]commontypes.MCMSWithTimelockConfig)
	for _, c := range e.Env.AllChainSelectors() {
		mcmsCfg[c] = cfg
	}
	var usdcChains []uint64
	if tCfg != nil && tCfg.IsUSDC {
		usdcChains = allChains
	}
	// Need to deploy prerequisites first so that we can form the USDC config
	// no proposals to be made, timelock can be passed as nil here
	e.Env, err = commonchangeset.ApplyChangesets(t, e.Env, nil, []commonchangeset.ChangesetApplication{
		{
			Changeset: commonchangeset.WrapChangeSet(DeployPrerequisites),
			Config: DeployPrerequisiteConfig{
				ChainSelectors: allChains,
				Opts: []PrerequisiteOpt{
					WithUSDCChains(usdcChains),
				},
			},
		},
		{
			Changeset: commonchangeset.WrapChangeSet(commonchangeset.DeployMCMSWithTimelock),
			Config:    mcmsCfg,
		},
	})
	require.NoError(t, err)

	state, err := LoadOnchainState(e.Env)
	require.NoError(t, err)
	tokenConfig := NewTestTokenConfig(state.Chains[e.FeedChainSel].USDFeeds)
	ocrParams := make(map[uint64]CCIPOCRParams)
	usdcCCTPConfig := make(map[cciptypes.ChainSelector]pluginconfig.USDCCCTPTokenConfig)
	timelocksPerChain := make(map[uint64]*gethwrappers.RBACTimelock)
	for _, chain := range usdcChains {
		require.NotNil(t, state.Chains[chain].MockUSDCTokenMessenger)
		require.NotNil(t, state.Chains[chain].MockUSDCTransmitter)
		require.NotNil(t, state.Chains[chain].USDCTokenPool)
		usdcCCTPConfig[cciptypes.ChainSelector(chain)] = pluginconfig.USDCCCTPTokenConfig{
			SourcePoolAddress:            state.Chains[chain].USDCTokenPool.Address().String(),
			SourceMessageTransmitterAddr: state.Chains[chain].MockUSDCTransmitter.Address().String(),
		}
	}
	for _, chain := range allChains {
		timelocksPerChain[chain] = state.Chains[chain].Timelock
		ocrParams[chain] = DefaultOCRParams(e.FeedChainSel, nil, nil)
	}
	var usdcCfg USDCAttestationConfig
	if len(usdcChains) > 0 {
		server := mockAttestationResponse()
		endpoint := server.URL
		usdcCfg = USDCAttestationConfig{
			API:         endpoint,
			APITimeout:  commonconfig.MustNewDuration(time.Second),
			APIInterval: commonconfig.MustNewDuration(500 * time.Millisecond),
		}
		t.Cleanup(func() {
			server.Close()
		})
	}

	// Deploy second set of changesets to deploy and configure the CCIP contracts.
	e.Env, err = commonchangeset.ApplyChangesets(t, e.Env, timelocksPerChain, []commonchangeset.ChangesetApplication{
		{
			Changeset: commonchangeset.WrapChangeSet(DeployChainContracts),
			Config: DeployChainContractsConfig{
				ChainSelectors:    allChains,
				HomeChainSelector: e.HomeChainSel,
			},
		},
		{
			Changeset: commonchangeset.WrapChangeSet(ConfigureNewChains),
			Config: NewChainsConfig{
				HomeChainSel:   e.HomeChainSel,
				FeedChainSel:   e.FeedChainSel,
				ChainsToDeploy: allChains,
				TokenConfig:    tokenConfig,
				OCRSecrets:     deployment.XXXGenerateTestOCRSecrets(),
				USDCConfig: USDCConfig{
					EnabledChains:         usdcChains,
					USDCAttestationConfig: usdcCfg,
					CCTPTokenConfig:       usdcCCTPConfig,
				},
				OCRParams: ocrParams,
			},
		},
		{
			Changeset: commonchangeset.WrapChangeSet(CCIPCapabilityJobspec),
		},
	})
	require.NoError(t, err)

	state, err = LoadOnchainState(e.Env)
	require.NoError(t, err)
	require.NotNil(t, state.Chains[e.HomeChainSel].CapabilityRegistry)
	require.NotNil(t, state.Chains[e.HomeChainSel].CCIPHome)
	require.NotNil(t, state.Chains[e.HomeChainSel].RMNHome)
	for _, chain := range allChains {
		require.NotNil(t, state.Chains[chain].LinkToken)
		require.NotNil(t, state.Chains[chain].Weth9)
		require.NotNil(t, state.Chains[chain].TokenAdminRegistry)
		require.NotNil(t, state.Chains[chain].RegistryModule)
		require.NotNil(t, state.Chains[chain].Router)
		require.NotNil(t, state.Chains[chain].RMNRemote)
		require.NotNil(t, state.Chains[chain].TestRouter)
		require.NotNil(t, state.Chains[chain].NonceManager)
		require.NotNil(t, state.Chains[chain].FeeQuoter)
		require.NotNil(t, state.Chains[chain].OffRamp)
		require.NotNil(t, state.Chains[chain].OnRamp)
	}
	return e
}

func CCIPSendRequest(
	e deployment.Environment,
	state CCIPOnChainState,
	src, dest uint64,
	testRouter bool,
	evm2AnyMessage router.ClientEVM2AnyMessage,
) (*types.Transaction, uint64, error) {
	msg := router.ClientEVM2AnyMessage{
		Receiver:     evm2AnyMessage.Receiver,
		Data:         evm2AnyMessage.Data,
		TokenAmounts: evm2AnyMessage.TokenAmounts,
		FeeToken:     evm2AnyMessage.FeeToken,
		ExtraArgs:    evm2AnyMessage.ExtraArgs,
	}
	r := state.Chains[src].Router
	if testRouter {
		r = state.Chains[src].TestRouter
	}
	fee, err := r.GetFee(
		&bind.CallOpts{Context: context.Background()}, dest, msg)
	if err != nil {
		return nil, 0, errors.Wrap(deployment.MaybeDataErr(err), "failed to get fee")
	}
	if msg.FeeToken == common.HexToAddress("0x0") {
		e.Chains[src].DeployerKey.Value = fee
		defer func() { e.Chains[src].DeployerKey.Value = nil }()
	}
	tx, err := r.CcipSend(
		e.Chains[src].DeployerKey,
		dest,
		msg)
	if err != nil {
		return nil, 0, errors.Wrap(err, "failed to send CCIP message")
	}
	blockNum, err := e.Chains[src].Confirm(tx)
	if err != nil {
		return tx, 0, errors.Wrap(err, "failed to confirm CCIP message")
	}
	return tx, blockNum, nil
}

// CCIPSendCalldata packs the calldata for the Router's ccipSend method.
// This is expected to be used in Multicall scenarios (i.e multiple ccipSend calls
// in a single transaction).
func CCIPSendCalldata(
	destChainSelector uint64,
	evm2AnyMessage router.ClientEVM2AnyMessage,
) ([]byte, error) {
	calldata, err := routerABI.Methods["ccipSend"].Inputs.Pack(
		destChainSelector,
		evm2AnyMessage,
	)
	if err != nil {
		return nil, fmt.Errorf("pack ccipSend calldata: %w", err)
	}

	calldata = append(routerABI.Methods["ccipSend"].ID, calldata...)
	return calldata, nil
}

func TestSendRequest(
	t *testing.T,
	e deployment.Environment,
	state CCIPOnChainState,
	src, dest uint64,
	testRouter bool,
	evm2AnyMessage router.ClientEVM2AnyMessage,
) (msgSentEvent *onramp.OnRampCCIPMessageSent) {
	t.Logf("Sending CCIP request from chain selector %d to chain selector %d",
		src, dest)
	tx, blockNum, err := CCIPSendRequest(
		e,
		state,
		src, dest,
		testRouter,
		evm2AnyMessage,
	)
	require.NoError(t, err)
	it, err := state.Chains[src].OnRamp.FilterCCIPMessageSent(&bind.FilterOpts{
		Start:   blockNum,
		End:     &blockNum,
		Context: context.Background(),
	}, []uint64{dest}, []uint64{})
	require.NoError(t, err)
	require.True(t, it.Next())
	t.Logf("CCIP message (id %x) sent from chain selector %d to chain selector %d tx %s seqNum %d nonce %d sender %s",
		it.Event.Message.Header.MessageId[:],
		src,
		dest,
		tx.Hash().String(),
		it.Event.SequenceNumber,
		it.Event.Message.Header.Nonce,
		it.Event.Message.Sender.String(),
	)
	return it.Event
}

// MakeEVMExtraArgsV2 creates the extra args for the EVM2Any message that is destined
// for an EVM chain. The extra args contain the gas limit and allow out of order flag.
func MakeEVMExtraArgsV2(gasLimit uint64, allowOOO bool) []byte {
	// extra args is the tag followed by the gas limit and allowOOO abi-encoded.
	var extraArgs []byte
	extraArgs = append(extraArgs, evmExtraArgsV2Tag...)
	gasLimitBytes := new(big.Int).SetUint64(gasLimit).Bytes()
	// pad from the left to 32 bytes
	gasLimitBytes = common.LeftPadBytes(gasLimitBytes, 32)

	// abi-encode allowOOO
	var allowOOOBytes []byte
	if allowOOO {
		allowOOOBytes = append(allowOOOBytes, 1)
	} else {
		allowOOOBytes = append(allowOOOBytes, 0)
	}
	// pad from the left to 32 bytes
	allowOOOBytes = common.LeftPadBytes(allowOOOBytes, 32)

	extraArgs = append(extraArgs, gasLimitBytes...)
	extraArgs = append(extraArgs, allowOOOBytes...)
	return extraArgs
}

// AddLanesForAll adds densely connected lanes for all chains in the environment so that each chain
// is connected to every other chain except itself.
func AddLanesForAll(e deployment.Environment, state CCIPOnChainState) error {
	for source := range e.Chains {
		for dest := range e.Chains {
			if source != dest {
				err := AddLaneWithDefaultPricesAndFeeQuoterConfig(e, state, source, dest, false)
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func ToPackedFee(execFee, daFee *big.Int) *big.Int {
	daShifted := new(big.Int).Lsh(daFee, 112)
	return new(big.Int).Or(daShifted, execFee)
}

const (
	// MockLinkAggregatorDescription This is the description of the MockV3Aggregator.sol contract
	// nolint:lll
	// https://github.com/smartcontractkit/chainlink/blob/a348b98e90527520049c580000a86fb8ceff7fa7/contracts/src/v0.8/tests/MockV3Aggregator.sol#L76-L76
	MockLinkAggregatorDescription = "v0.8/tests/MockV3Aggregator.sol"
	// MockWETHAggregatorDescription WETH use description from MockETHUSDAggregator.sol
	// nolint:lll
	// https://github.com/smartcontractkit/chainlink/blob/a348b98e90527520049c580000a86fb8ceff7fa7/contracts/src/v0.8/automation/testhelpers/MockETHUSDAggregator.sol#L19-L19
	MockWETHAggregatorDescription = "MockETHUSDAggregator"
)

var (
	MockLinkPrice = deployment.E18Mult(500)
	MockWethPrice = big.NewInt(9e8)
	// MockDescriptionToTokenSymbol maps a mock feed description to token descriptor
	MockDescriptionToTokenSymbol = map[string]TokenSymbol{
		MockLinkAggregatorDescription: LinkSymbol,
		MockWETHAggregatorDescription: WethSymbol,
	}
	MockSymbolToDescription = map[TokenSymbol]string{
		LinkSymbol: MockLinkAggregatorDescription,
		WethSymbol: MockWETHAggregatorDescription,
	}
	MockSymbolToDecimals = map[TokenSymbol]uint8{
		LinkSymbol: LinkDecimals,
		WethSymbol: WethDecimals,
	}
)

func DeployFeeds(
	lggr logger.Logger,
	ab deployment.AddressBook,
	chain deployment.Chain,
	linkPrice *big.Int,
	wethPrice *big.Int,
) (map[string]common.Address, error) {
	linkTV := deployment.NewTypeAndVersion(PriceFeed, deployment.Version1_0_0)
	mockLinkFeed := func(chain deployment.Chain) deployment.ContractDeploy[*aggregator_v3_interface.AggregatorV3Interface] {
		linkFeed, tx, _, err1 := mock_v3_aggregator_contract.DeployMockV3Aggregator(
			chain.DeployerKey,
			chain.Client,
			LinkDecimals, // decimals
			linkPrice,    // initialAnswer
		)
		aggregatorCr, err2 := aggregator_v3_interface.NewAggregatorV3Interface(linkFeed, chain.Client)

		return deployment.ContractDeploy[*aggregator_v3_interface.AggregatorV3Interface]{
			Address: linkFeed, Contract: aggregatorCr, Tv: linkTV, Tx: tx, Err: multierr.Append(err1, err2),
		}
	}

	mockWethFeed := func(chain deployment.Chain) deployment.ContractDeploy[*aggregator_v3_interface.AggregatorV3Interface] {
		wethFeed, tx, _, err1 := mock_ethusd_aggregator_wrapper.DeployMockETHUSDAggregator(
			chain.DeployerKey,
			chain.Client,
			wethPrice, // initialAnswer
		)
		aggregatorCr, err2 := aggregator_v3_interface.NewAggregatorV3Interface(wethFeed, chain.Client)

		return deployment.ContractDeploy[*aggregator_v3_interface.AggregatorV3Interface]{
			Address: wethFeed, Contract: aggregatorCr, Tv: linkTV, Tx: tx, Err: multierr.Append(err1, err2),
		}
	}

	linkFeedAddress, linkFeedDescription, err := deploySingleFeed(lggr, ab, chain, mockLinkFeed, LinkSymbol)
	if err != nil {
		return nil, err
	}

	wethFeedAddress, wethFeedDescription, err := deploySingleFeed(lggr, ab, chain, mockWethFeed, WethSymbol)
	if err != nil {
		return nil, err
	}

	descriptionToAddress := map[string]common.Address{
		linkFeedDescription: linkFeedAddress,
		wethFeedDescription: wethFeedAddress,
	}

	return descriptionToAddress, nil
}

func deploySingleFeed(
	lggr logger.Logger,
	ab deployment.AddressBook,
	chain deployment.Chain,
	deployFunc func(deployment.Chain) deployment.ContractDeploy[*aggregator_v3_interface.AggregatorV3Interface],
	symbol TokenSymbol,
) (common.Address, string, error) {
	//tokenTV := deployment.NewTypeAndVersion(PriceFeed, deployment.Version1_0_0)
	mockTokenFeed, err := deployment.DeployContract(lggr, chain, ab, deployFunc)
	if err != nil {
		lggr.Errorw("Failed to deploy token feed", "err", err, "symbol", symbol)
		return common.Address{}, "", err
	}

	lggr.Infow("deployed mockTokenFeed", "addr", mockTokenFeed.Address)

	desc, err := mockTokenFeed.Contract.Description(&bind.CallOpts{})
	if err != nil {
		lggr.Errorw("Failed to get description", "err", err, "symbol", symbol)
		return common.Address{}, "", err
	}

	if desc != MockSymbolToDescription[symbol] {
		lggr.Errorw("Unexpected description for token", "symbol", symbol, "desc", desc)
		return common.Address{}, "", fmt.Errorf("unexpected description: %s", desc)
	}

	return mockTokenFeed.Address, desc, nil
}

func ConfirmRequestOnSourceAndDest(t *testing.T, env deployment.Environment, state CCIPOnChainState, sourceCS, destCS, expectedSeqNr uint64) error {
	latesthdr, err := env.Chains[destCS].Client.HeaderByNumber(testcontext.Get(t), nil)
	require.NoError(t, err)
	startBlock := latesthdr.Number.Uint64()
	fmt.Printf("startblock %d", startBlock)
	msgSentEvent := TestSendRequest(t, env, state, sourceCS, destCS, false, router.ClientEVM2AnyMessage{
		Receiver:     common.LeftPadBytes(state.Chains[destCS].Receiver.Address().Bytes(), 32),
		Data:         []byte("hello world"),
		TokenAmounts: nil,
		FeeToken:     common.HexToAddress("0x0"),
		ExtraArgs:    nil,
	})
	require.Equal(t, expectedSeqNr, msgSentEvent.SequenceNumber)

	fmt.Printf("Request sent for seqnr %d", msgSentEvent.SequenceNumber)
	require.NoError(t,
		commonutils.JustError(ConfirmCommitWithExpectedSeqNumRange(t, env.Chains[sourceCS], env.Chains[destCS], state.Chains[destCS].OffRamp, &startBlock, cciptypes.SeqNumRange{
			cciptypes.SeqNum(msgSentEvent.SequenceNumber),
			cciptypes.SeqNum(msgSentEvent.SequenceNumber),
		})))

	fmt.Printf("Commit confirmed for seqnr %d", msgSentEvent.SequenceNumber)
	require.NoError(
		t,
		commonutils.JustError(
			ConfirmExecWithSeqNrs(
				t,
				env.Chains[sourceCS],
				env.Chains[destCS],
				state.Chains[destCS].OffRamp,
				&startBlock,
				[]uint64{msgSentEvent.SequenceNumber},
			),
		),
	)

	return nil
}

// TODO: Remove this to replace with ApplyChangeset
func ProcessChangeset(t *testing.T, e deployment.Environment, c deployment.ChangesetOutput) {

	// TODO: Add support for jobspecs as well

	// sign and execute all proposals provided
	if len(c.Proposals) != 0 {
		state, err := LoadOnchainState(e)
		require.NoError(t, err)
		for _, prop := range c.Proposals {
			chains := mapset.NewSet[uint64]()
			for _, op := range prop.Transactions {
				chains.Add(uint64(op.ChainIdentifier))
			}

			signed := commonchangeset.SignProposal(t, e, &prop)
			for _, sel := range chains.ToSlice() {
				commonchangeset.ExecuteProposal(t, e, signed, state.Chains[sel].Timelock, sel)
			}
		}
	}

	// merge address books
	if c.AddressBook != nil {
		err := e.ExistingAddresses.Merge(c.AddressBook)
		require.NoError(t, err)
	}
}

func DeployTransferableToken(
	lggr logger.Logger,
	chains map[uint64]deployment.Chain,
	src, dst uint64,
	state CCIPOnChainState,
	addresses deployment.AddressBook,
	token string,
) (*burn_mint_erc677.BurnMintERC677, *burn_mint_token_pool.BurnMintTokenPool, *burn_mint_erc677.BurnMintERC677, *burn_mint_token_pool.BurnMintTokenPool, error) {
	// Deploy token and pools
	srcToken, srcPool, err := deployTransferTokenOneEnd(lggr, chains[src], addresses, token)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	dstToken, dstPool, err := deployTransferTokenOneEnd(lggr, chains[dst], addresses, token)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	// Attach token pools to registry
	if err := attachTokenToTheRegistry(chains[src], state.Chains[src], chains[src].DeployerKey, srcToken.Address(), srcPool.Address()); err != nil {
		return nil, nil, nil, nil, err
	}

	if err := attachTokenToTheRegistry(chains[dst], state.Chains[dst], chains[dst].DeployerKey, dstToken.Address(), dstPool.Address()); err != nil {
		return nil, nil, nil, nil, err
	}

	// Connect pool to each other
	if err := setTokenPoolCounterPart(chains[src], srcPool, dst, dstToken.Address(), dstPool.Address()); err != nil {
		return nil, nil, nil, nil, err
	}

	if err := setTokenPoolCounterPart(chains[dst], dstPool, src, srcToken.Address(), srcPool.Address()); err != nil {
		return nil, nil, nil, nil, err
	}

	// Add burn/mint permissions
	if err := grantMintBurnPermissions(lggr, chains[src], srcToken, srcPool.Address()); err != nil {
		return nil, nil, nil, nil, err
	}

	if err := grantMintBurnPermissions(lggr, chains[dst], dstToken, dstPool.Address()); err != nil {
		return nil, nil, nil, nil, err
	}

	return srcToken, srcPool, dstToken, dstPool, nil
}

func grantMintBurnPermissions(lggr logger.Logger, chain deployment.Chain, token *burn_mint_erc677.BurnMintERC677, address common.Address) error {
	lggr.Infow("Granting burn permissions", "token", token.Address(), "burner", address)
	tx, err := token.GrantBurnRole(chain.DeployerKey, address)
	if err != nil {
		return err
	}
	_, err = chain.Confirm(tx)
	if err != nil {
		return err
	}

	lggr.Infow("Granting mint permissions", "token", token.Address(), "minter", address)
	tx, err = token.GrantMintRole(chain.DeployerKey, address)
	if err != nil {
		return err
	}
	_, err = chain.Confirm(tx)
	return err
}

func setUSDCTokenPoolCounterPart(
	chain deployment.Chain,
	tokenPool *usdc_token_pool.USDCTokenPool,
	destChainSelector uint64,
	destTokenAddress common.Address,
	destTokenPoolAddress common.Address,
) error {
	allowedCaller := common.LeftPadBytes(destTokenPoolAddress.Bytes(), 32)
	var fixedAddr [32]byte
	copy(fixedAddr[:], allowedCaller[:32])

	domain, _ := reader.AllAvailableDomains()[destChainSelector]

	domains := []usdc_token_pool.USDCTokenPoolDomainUpdate{
		{
			AllowedCaller:     fixedAddr,
			DomainIdentifier:  domain,
			DestChainSelector: destChainSelector,
			Enabled:           true,
		},
	}
	tx, err := tokenPool.SetDomains(chain.DeployerKey, domains)
	if err != nil {
		return err
	}

	_, err = chain.Confirm(tx)
	if err != nil {
		return err
	}

	pool, err := burn_mint_token_pool.NewBurnMintTokenPool(tokenPool.Address(), chain.Client)
	if err != nil {
		return err
	}

	return setTokenPoolCounterPart(chain, pool, destChainSelector, destTokenAddress, destTokenPoolAddress)
}

func setTokenPoolCounterPart(
	chain deployment.Chain,
	tokenPool *burn_mint_token_pool.BurnMintTokenPool,
	destChainSelector uint64,
	destTokenAddress common.Address,
	destTokenPoolAddress common.Address,
) error {
	tx, err := tokenPool.ApplyChainUpdates(
		chain.DeployerKey,
		[]uint64{},
		[]burn_mint_token_pool.TokenPoolChainUpdate{
			{
				RemoteChainSelector: destChainSelector,
				RemotePoolAddresses: [][]byte{common.LeftPadBytes(destTokenPoolAddress.Bytes(), 32)},
				RemoteTokenAddress:  common.LeftPadBytes(destTokenAddress.Bytes(), 32),
				OutboundRateLimiterConfig: burn_mint_token_pool.RateLimiterConfig{
					IsEnabled: false,
					Capacity:  big.NewInt(0),
					Rate:      big.NewInt(0),
				},
				InboundRateLimiterConfig: burn_mint_token_pool.RateLimiterConfig{
					IsEnabled: false,
					Capacity:  big.NewInt(0),
					Rate:      big.NewInt(0),
				},
			},
		},
	)
	if err != nil {
		return fmt.Errorf("failed to apply chain updates on token pool %s: %w", tokenPool.Address(), err)
	}

	_, err = chain.Confirm(tx)
	if err != nil {
		return err
	}

	tx, err = tokenPool.AddRemotePool(
		chain.DeployerKey,
		destChainSelector,
		destTokenPoolAddress.Bytes(),
	)
	if err != nil {
		return fmt.Errorf("failed to set remote pool on token pool %s: %w", tokenPool.Address(), err)
	}

	_, err = chain.Confirm(tx)
	return err
}

func attachTokenToTheRegistry(
	chain deployment.Chain,
	state CCIPChainState,
	owner *bind.TransactOpts,
	token common.Address,
	tokenPool common.Address,
) error {
	pool, err := state.TokenAdminRegistry.GetPool(nil, token)
	if err != nil {
		return err
	}
	// Pool is already registered, don't reattach it, because it would cause revert
	if pool != (common.Address{}) {
		return nil
	}

	tx, err := state.RegistryModule.RegisterAdminViaOwner(owner, token)
	if err != nil {
		return err
	}
	_, err = chain.Confirm(tx)
	if err != nil {
		return err
	}

	tx, err = state.TokenAdminRegistry.AcceptAdminRole(owner, token)
	if err != nil {
		return err
	}
	_, err = chain.Confirm(tx)
	if err != nil {
		return err
	}

	tx, err = state.TokenAdminRegistry.SetPool(owner, token, tokenPool)
	if err != nil {
		return err
	}
	_, err = chain.Confirm(tx)
	if err != nil {
		return err
	}
	return nil
}

func deployTransferTokenOneEnd(
	lggr logger.Logger,
	chain deployment.Chain,
	addressBook deployment.AddressBook,
	tokenSymbol string,
) (*burn_mint_erc677.BurnMintERC677, *burn_mint_token_pool.BurnMintTokenPool, error) {
	var rmnAddress, routerAddress string
	chainAddresses, err := addressBook.AddressesForChain(chain.Selector)
	if err != nil {
		return nil, nil, err
	}
	for address, v := range chainAddresses {
		if deployment.NewTypeAndVersion(ARMProxy, deployment.Version1_0_0) == v {
			rmnAddress = address
		}
		if deployment.NewTypeAndVersion(Router, deployment.Version1_2_0) == v {
			routerAddress = address
		}
		if rmnAddress != "" && routerAddress != "" {
			break
		}
	}

	tokenDecimals := uint8(18)

	tokenContract, err := deployment.DeployContract(lggr, chain, addressBook,
		func(chain deployment.Chain) deployment.ContractDeploy[*burn_mint_erc677.BurnMintERC677] {
			USDCTokenAddr, tx, token, err2 := burn_mint_erc677.DeployBurnMintERC677(
				chain.DeployerKey,
				chain.Client,
				tokenSymbol,
				tokenSymbol,
				tokenDecimals,
				big.NewInt(0).Mul(big.NewInt(1e9), big.NewInt(1e18)),
			)
			return deployment.ContractDeploy[*burn_mint_erc677.BurnMintERC677]{
				USDCTokenAddr, token, tx, deployment.NewTypeAndVersion(BurnMintToken, deployment.Version1_0_0), err2,
			}
		})
	if err != nil {
		lggr.Errorw("Failed to deploy Token ERC677", "err", err)
		return nil, nil, err
	}

	tx, err := tokenContract.Contract.GrantMintRole(chain.DeployerKey, chain.DeployerKey.From)
	if err != nil {
		return nil, nil, err
	}
	_, err = chain.Confirm(tx)
	if err != nil {
		return nil, nil, err
	}

	tokenPool, err := deployment.DeployContract(lggr, chain, addressBook,
		func(chain deployment.Chain) deployment.ContractDeploy[*burn_mint_token_pool.BurnMintTokenPool] {
			tokenPoolAddress, tx, tokenPoolContract, err2 := burn_mint_token_pool.DeployBurnMintTokenPool(
				chain.DeployerKey,
				chain.Client,
				tokenContract.Address,
				tokenDecimals,
				[]common.Address{},
				common.HexToAddress(rmnAddress),
				common.HexToAddress(routerAddress),
			)
			return deployment.ContractDeploy[*burn_mint_token_pool.BurnMintTokenPool]{
				tokenPoolAddress, tokenPoolContract, tx, deployment.NewTypeAndVersion(BurnMintTokenPool, deployment.Version1_0_0), err2,
			}
		})
	if err != nil {
		lggr.Errorw("Failed to deploy token pool", "err", err)
		return nil, nil, err
	}

	return tokenContract.Contract, tokenPool.Contract, nil
}
