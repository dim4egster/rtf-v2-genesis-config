package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"io/ioutil"
	"math/big"
	"os"
	"reflect"
	"strings"
	"unicode"
	"unsafe"

	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/core/systemcontracts"

	_ "github.com/ethereum/go-ethereum/eth/tracers/native"

	"github.com/ethereum/go-ethereum/crypto"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/trie"
)

type artifactData struct {
	Bytecode         string `json:"bytecode"`
	DeployedBytecode string `json:"deployedBytecode"`
}

type dummyChainContext struct {
}

func (d *dummyChainContext) Engine() consensus.Engine {
	return nil
}

func (d *dummyChainContext) GetHeader(common.Hash, uint64) *types.Header {
	return nil
}

func createExtraData(validators []common.Address) []byte {
	extra := make([]byte, 32+20*len(validators)+65)
	for i, v := range validators {
		copy(extra[32+20*i:], v.Bytes())
	}
	return extra
}

func readDirtyStorageFromState(f interface{}) state.Storage {
	var result map[common.Hash]common.Hash
	rs := reflect.ValueOf(f).Elem()
	rf := rs.FieldByName("dirtyStorage")
	rs2 := reflect.New(rs.Type()).Elem()
	rs2.Set(rs)
	rf = rs2.FieldByName("dirtyStorage")
	rf = reflect.NewAt(rf.Type(), unsafe.Pointer(rf.UnsafeAddr())).Elem()
	ri := reflect.ValueOf(&result).Elem()
	ri.Set(rf)
	return result
}

func simulateSystemContract(genesis *core.Genesis, systemContract common.Address, rawArtifact []byte, constructor []byte, balance *big.Int) error {
	artifact := &artifactData{}
	if err := json.Unmarshal(rawArtifact, artifact); err != nil {
		return err
	}
	bytecode := append(hexutil.MustDecode(artifact.Bytecode), constructor...)
	// simulate constructor execution
	ethdb := rawdb.NewDatabase(memorydb.New())
	db := state.NewDatabaseWithConfig(ethdb, &trie.Config{})
	statedb, err := state.New(common.Hash{}, db, nil)
	if err != nil {
		return err
	}
	statedb.SetBalance(systemContract, balance)
	block := genesis.ToBlock()
	blockContext := core.NewEVMBlockContext(block.Header(), &dummyChainContext{}, &common.Address{})

	msg := &core.Message{
		To:                &common.Address{},
		From:              systemContract,
		Value:             big.NewInt(0),
		GasLimit:          10_000_000,
		GasPrice:          big.NewInt(0),
		GasFeeCap:         big.NewInt(0),
		GasTipCap:         big.NewInt(0),
		Data:              []byte{},
		SkipAccountChecks: false}

	txContext := core.NewEVMTxContext(msg)

	if err != nil {
		return err
	}
	evm := vm.NewEVM(blockContext, txContext, statedb, genesis.Config, vm.Config{})
	deployedBytecode, _, err := evm.CreateWithAddress(vm.AccountRef(common.Address{}), bytecode, 10_000_000, big.NewInt(0), systemContract)
	if err != nil {
		for _, c := range deployedBytecode[64:] {
			if c >= 32 && c <= unicode.MaxASCII {
				print(string(c))
			}
		}
		println()
		return err
	}
	storage := readDirtyStorageFromState(statedb.GetOrNewStateObject(systemContract))
	// read state changes from state database
	genesisAccount := core.GenesisAccount{
		Code:    deployedBytecode,
		Storage: storage.Copy(),
		Balance: big.NewInt(0),
		Nonce:   0,
	}
	if genesis.Alloc == nil {
		genesis.Alloc = make(core.GenesisAlloc)
	}
	genesis.Alloc[systemContract] = genesisAccount
	// make sure ctor working fine (better to fail here instead of in consensus engine)
	errorCode, _, err := evm.Call(vm.AccountRef(common.Address{}), systemContract, hexutil.MustDecode("0xe1c7392a"), 10_000_000, big.NewInt(0))
	if err != nil {
		for _, c := range errorCode[64:] {
			if c >= 32 && c <= unicode.MaxASCII {
				print(string(c))
			}
		}
		println()
		return err
	}
	return nil
}

var stakingAddress = common.HexToAddress("0x0000000000000000000000000000000000001000")
var slashingIndicatorAddress = common.HexToAddress("0x0000000000000000000000000000000000001001")
var systemRewardAddress = common.HexToAddress("0x0000000000000000000000000000000000001002")
var stakingPoolAddress = common.HexToAddress("0x0000000000000000000000000000000000007001")
var governanceAddress = common.HexToAddress("0x0000000000000000000000000000000000007002")
var chainConfigAddress = common.HexToAddress("0x0000000000000000000000000000000000007003")
var runtimeUpgradeAddress = common.HexToAddress("0x0000000000000000000000000000000000007004")
var deployerProxyAddress = common.HexToAddress("0x0000000000000000000000000000000000007005")
var intermediarySystemAddress = common.HexToAddress("0xfffffffffffffffffffffffffffffffffffffffe")

//go:embed build/contracts/Staking.json
var stakingRawArtifact []byte

//go:embed build/contracts/StakingPool.json
var stakingPoolRawArtifact []byte

//go:embed build/contracts/ChainConfig.json
var chainConfigRawArtifact []byte

//go:embed build/contracts/SlashingIndicator.json
var slashingIndicatorRawArtifact []byte

//go:embed build/contracts/SystemReward.json
var systemRewardRawArtifact []byte

//go:embed build/contracts/Governance.json
var governanceRawArtifact []byte

//go:embed build/contracts/RuntimeUpgrade.json
var runtimeUpgradeRawArtifact []byte

//go:embed build/contracts/DeployerProxy.json
var deployerProxyRawArtifact []byte

func newArguments(typeNames ...string) abi.Arguments {
	var args abi.Arguments
	for i, tn := range typeNames {
		abiType, err := abi.NewType(tn, tn, nil)
		if err != nil {
			panic(err)
		}
		args = append(args, abi.Argument{Name: fmt.Sprintf("%d", i), Type: abiType})
	}
	return args
}

type consensusParams struct {
	ActiveValidatorsLength   uint32                `json:"activeValidatorsLength"`
	EpochBlockInterval       uint32                `json:"epochBlockInterval"`
	MisdemeanorThreshold     uint32                `json:"misdemeanorThreshold"`
	FelonyThreshold          uint32                `json:"felonyThreshold"`
	ValidatorJailEpochLength uint32                `json:"validatorJailEpochLength"`
	UndelegatePeriod         uint32                `json:"undelegatePeriod"`
	MinValidatorStakeAmount  *math.HexOrDecimal256 `json:"minValidatorStakeAmount"`
	MinStakingAmount         *math.HexOrDecimal256 `json:"minStakingAmount"`
}

type RTFForks struct {
	RuntimeUpgradeBlock    *math.HexOrDecimal256 `json:"runtimeUpgradeBlock"`
	DeployOriginBlock      *math.HexOrDecimal256 `json:"deployOriginBlock"`
	DeploymentHookFixBlock *math.HexOrDecimal256 `json:"deploymentHookFixBlock"`
}

type genesisConfig struct {
	ChainId         int64                     `json:"chainId"`
	Deployers       []common.Address          `json:"deployers"`
	Validators      []common.Address          `json:"validators"`
	SystemTreasury  map[common.Address]uint16 `json:"systemTreasury"`
	ConsensusParams consensusParams           `json:"consensusParams"`
	VotingPeriod    int64                     `json:"votingPeriod"`
	Faucet          map[common.Address]string `json:"faucet"`
	CommissionRate  int64                     `json:"commissionRate"`
	InitialStakes   map[common.Address]string `json:"initialStakes"`
	Forks           RTFForks                  `json:"forks"`
}

func invokeConstructorOrPanic(genesis *core.Genesis, contract common.Address, rawArtifact []byte, typeNames []string, params []interface{}, silent bool, balance *big.Int) {
	ctor, err := newArguments(typeNames...).Pack(params...)
	if err != nil {
		panic(err)
	}
	sig := crypto.Keccak256([]byte(fmt.Sprintf("ctor(%s)", strings.Join(typeNames, ","))))[:4]
	ctor = append(sig, ctor...)
	ctor, err = newArguments("bytes").Pack(ctor)
	if err != nil {
		panic(err)
	}
	if !silent {
		fmt.Printf(" + calling constructor: address=%s sig=%s ctor=%s\n", contract.Hex(), hexutil.Encode(sig), hexutil.Encode(ctor))
	}
	if err := simulateSystemContract(genesis, contract, rawArtifact, ctor, balance); err != nil {
		panic(err)
	}
}

func createGenesisConfig(config genesisConfig, targetFile string) error {
	genesis := defaultGenesisConfig(config)
	// extra data
	genesis.ExtraData = createExtraData(config.Validators)
	genesis.Config.Parlia.Epoch = uint64(config.ConsensusParams.EpochBlockInterval)
	// execute system contracts
	var initialStakes []*big.Int
	initialStakeTotal := big.NewInt(0)
	for _, v := range config.Validators {
		rawInitialStake, ok := config.InitialStakes[v]
		if !ok {
			return fmt.Errorf("initial stake is not found for validator: %s", v.Hex())
		}
		initialStake, err := hexutil.DecodeBig(rawInitialStake)
		if err != nil {
			return err
		}
		initialStakes = append(initialStakes, initialStake)
		initialStakeTotal.Add(initialStakeTotal, initialStake)
	}
	silent := targetFile == "stdout"
	invokeConstructorOrPanic(genesis, stakingAddress, stakingRawArtifact, []string{"address[]", "uint256[]", "uint16"}, []interface{}{
		config.Validators,
		initialStakes,
		uint16(config.CommissionRate),
	}, silent, initialStakeTotal)
	invokeConstructorOrPanic(genesis, chainConfigAddress, chainConfigRawArtifact, []string{"uint32", "uint32", "uint32", "uint32", "uint32", "uint32", "uint256", "uint256"}, []interface{}{
		config.ConsensusParams.ActiveValidatorsLength,
		config.ConsensusParams.EpochBlockInterval,
		config.ConsensusParams.MisdemeanorThreshold,
		config.ConsensusParams.FelonyThreshold,
		config.ConsensusParams.ValidatorJailEpochLength,
		config.ConsensusParams.UndelegatePeriod,
		(*big.Int)(config.ConsensusParams.MinValidatorStakeAmount),
		(*big.Int)(config.ConsensusParams.MinStakingAmount),
	}, silent, nil)
	invokeConstructorOrPanic(genesis, slashingIndicatorAddress, slashingIndicatorRawArtifact, []string{}, []interface{}{}, silent, nil)
	invokeConstructorOrPanic(genesis, stakingPoolAddress, stakingPoolRawArtifact, []string{}, []interface{}{}, silent, nil)
	var treasuryAddresses []common.Address
	var treasuryShares []uint16
	for k, v := range config.SystemTreasury {
		treasuryAddresses = append(treasuryAddresses, k)
		treasuryShares = append(treasuryShares, v)
	}
	invokeConstructorOrPanic(genesis, systemRewardAddress, systemRewardRawArtifact, []string{"address[]", "uint16[]"}, []interface{}{
		treasuryAddresses, treasuryShares,
	}, silent, nil)
	invokeConstructorOrPanic(genesis, governanceAddress, governanceRawArtifact, []string{"uint256"}, []interface{}{
		big.NewInt(config.VotingPeriod),
	}, silent, nil)
	invokeConstructorOrPanic(genesis, runtimeUpgradeAddress, runtimeUpgradeRawArtifact, []string{"address"}, []interface{}{
		systemcontracts.EvmHookRuntimeUpgradeAddress,
	}, silent, nil)
	invokeConstructorOrPanic(genesis, deployerProxyAddress, deployerProxyRawArtifact, []string{"address[]"}, []interface{}{
		config.Deployers,
	}, silent, nil)
	// create system contract
	genesis.Alloc[intermediarySystemAddress] = core.GenesisAccount{
		Balance: big.NewInt(0),
	}
	// set staking allocation
	stakingAlloc := genesis.Alloc[stakingAddress]
	stakingAlloc.Balance = initialStakeTotal
	genesis.Alloc[stakingAddress] = stakingAlloc
	// apply faucet
	for key, value := range config.Faucet {
		balance, ok := new(big.Int).SetString(value[2:], 16)
		if !ok {
			return fmt.Errorf("failed to parse number (%s)", value)
		}
		genesis.Alloc[key] = core.GenesisAccount{
			Balance: balance,
		}
	}
	// save to file
	newJson, _ := json.MarshalIndent(genesis, "", "  ")
	if targetFile == "stdout" {
		_, err := os.Stdout.Write(newJson)
		return err
	} else if targetFile == "stderr" {
		_, err := os.Stderr.Write(newJson)
		return err
	}
	return ioutil.WriteFile(targetFile, newJson, fs.ModePerm)
}

func decimalToBigInt(value *math.HexOrDecimal256) *big.Int {
	if value == nil {
		return nil
	}
	return (*big.Int)(value)
}

func u64(val uint64) *uint64 { return &val }

func defaultGenesisConfig(config genesisConfig) *core.Genesis {
	chainConfig := &params.ChainConfig{
		ChainID: big.NewInt(config.ChainId),
		// Default ETH forks
		HomesteadBlock:      big.NewInt(0),
		EIP150Block:         big.NewInt(0),
		EIP155Block:         big.NewInt(0),
		EIP158Block:         big.NewInt(0),
		ByzantiumBlock:      big.NewInt(0),
		ConstantinopleBlock: big.NewInt(0),
		PetersburgBlock:     big.NewInt(0),
		IstanbulBlock:       big.NewInt(0),
		MuirGlacierBlock:    big.NewInt(0),
		RamanujanBlock:      big.NewInt(0),
		NielsBlock:          big.NewInt(0),
		MirrorSyncBlock:     big.NewInt(0),
		BrunoBlock:          big.NewInt(0),

		EulerBlock:   big.NewInt(0),
		NanoBlock:    big.NewInt(0),
		MoranBlock:   big.NewInt(0),
		GibbsBlock:   big.NewInt(0),
		PlanckBlock:  big.NewInt(0),
		BerlinBlock:  big.NewInt(0),
		LondonBlock:  big.NewInt(0),
		HertzBlock:   big.NewInt(0),
		ShanghaiTime: u64(0),
		// RTF V2 forks
		RuntimeUpgradeBlock:    decimalToBigInt(config.Forks.RuntimeUpgradeBlock),
		DeployOriginBlock:      decimalToBigInt(config.Forks.DeployOriginBlock),
		DeploymentHookFixBlock: decimalToBigInt(config.Forks.DeploymentHookFixBlock),
		// Parlia config
		Parlia: &params.ParliaConfig{
			Period: 3,
			// epoch length is managed by consensus params
		},
	}
	return &core.Genesis{
		Config:     chainConfig,
		Nonce:      0,
		Timestamp:  0x65CF9B5C,
		ExtraData:  nil,
		GasLimit:   0x2625a00,
		Difficulty: big.NewInt(0x01),
		Mixhash:    common.Hash{},
		Coinbase:   common.Address{},
		Alloc:      nil,
		Number:     0x00,
		GasUsed:    0x00,
		ParentHash: common.Hash{},
	}
}

var localNetConfig = genesisConfig{
	ChainId: 1337,
	// who is able to deploy smart contract from genesis block
	Deployers: []common.Address{
		common.HexToAddress("0x00a601f45688dba8a070722073b015277cf36725"),
	},
	// list of default validators
	Validators: []common.Address{
		common.HexToAddress("0x00a601f45688dba8a070722073b015277cf36725"),
	},
	SystemTreasury: map[common.Address]uint16{
		common.HexToAddress("0x00a601f45688dba8a070722073b015277cf36725"): 10000,
	},
	ConsensusParams: consensusParams{
		ActiveValidatorsLength:   25,                                                                    // suggested values are (3k+1, where k is honest validators, even better): 7, 13, 19, 25, 31...
		EpochBlockInterval:       40,                                                                    // better to use 1 day epoch (86400/3=28800, where 3s is block time)
		MisdemeanorThreshold:     5,                                                                     // after missing this amount of blocks per day validator losses all daily rewards (penalty)
		FelonyThreshold:          10,                                                                    // after missing this amount of blocks per day validator goes in jail for N epochs
		ValidatorJailEpochLength: 3,                                                                     // how many epochs validator should stay in jail (7 epochs = ~7 days)
		UndelegatePeriod:         2,                                                                     // allow claiming funds only after 6 epochs (~7 days)
		MinValidatorStakeAmount:  (*math.HexOrDecimal256)(hexutil.MustDecodeBig("0xde0b6b3a7640000")),   // 1 ether
		MinStakingAmount:         (*math.HexOrDecimal256)(hexutil.MustDecodeBig("0x1bc16d674ec800002")), // 1 ether
	},
	InitialStakes: map[common.Address]string{
		common.HexToAddress("0x00a601f45688dba8a070722073b015277cf36725"): "0x3635c9adc5dea00000", // 1000 eth
	},
	// owner of the governance
	VotingPeriod: 20, // 1 minute
	// faucet
	Faucet: map[common.Address]string{
		common.HexToAddress("0x00a601f45688dba8a070722073b015277cf36725"): "0x21e19e0c9bab2400000",
		common.HexToAddress("0x57BA24bE2cF17400f37dB3566e839bfA6A2d018a"): "0x21e19e0c9bab2400000",
		common.HexToAddress("0xEbCf9D06cf9333706E61213F17A795B2F7c55F1b"): "0x21e19e0c9bab2400000",
	},
}

var devNetConfig = genesisConfig{
	ChainId: 17243,
	// who is able to deploy smart contract from genesis block (it won't generate event log)
	Deployers: []common.Address{},
	// list of default validators (it won't generate event log)
	Validators: []common.Address{
		common.HexToAddress("0x08fae3885e299c24ff9841478eb946f41023ac69"),
		common.HexToAddress("0x751aaca849b09a3e347bbfe125cf18423cc24b40"),
		common.HexToAddress("0xa6ff33e3250cc765052ac9d7f7dfebda183c4b9b"),
		common.HexToAddress("0x49c0f7c8c11a4c80dc6449efe1010bb166818da8"),
		common.HexToAddress("0x8e1ea6eaa09c3b40f4a51fcd056a031870a0549a"),
	},
	SystemTreasury: map[common.Address]uint16{
		common.HexToAddress("0x0000000000000000000000000000000000000000"): 10000,
	},
	ConsensusParams: consensusParams{
		ActiveValidatorsLength:   25,   // suggested values are (3k+1, where k is honest validators, even better): 7, 13, 19, 25, 31...
		EpochBlockInterval:       1200, // better to use 1 day epoch (86400/3=28800, where 3s is block time)
		MisdemeanorThreshold:     50,   // after missing this amount of blocks per day validator losses all daily rewards (penalty)
		FelonyThreshold:          150,  // after missing this amount of blocks per day validator goes in jail for N epochs
		ValidatorJailEpochLength: 7,    // how many epochs validator should stay in jail (7 epochs = ~7 days)
		UndelegatePeriod:         6,    // allow claiming funds only after 6 epochs (~7 days)

		MinValidatorStakeAmount: (*math.HexOrDecimal256)(hexutil.MustDecodeBig("0xde0b6b3a7640000")), // how many tokens validator must stake to create a validator (in ether)
		MinStakingAmount:        (*math.HexOrDecimal256)(hexutil.MustDecodeBig("0xde0b6b3a7640000")), // minimum staking amount for delegators (in ether)
	},
	InitialStakes: map[common.Address]string{
		common.HexToAddress("0x08fae3885e299c24ff9841478eb946f41023ac69"): "0x3635c9adc5dea00000", // 1000 eth
		common.HexToAddress("0x751aaca849b09a3e347bbfe125cf18423cc24b40"): "0x3635c9adc5dea00000", // 1000 eth
		common.HexToAddress("0xa6ff33e3250cc765052ac9d7f7dfebda183c4b9b"): "0x3635c9adc5dea00000", // 1000 eth
		common.HexToAddress("0x49c0f7c8c11a4c80dc6449efe1010bb166818da8"): "0x3635c9adc5dea00000", // 1000 eth
		common.HexToAddress("0x8e1ea6eaa09c3b40f4a51fcd056a031870a0549a"): "0x3635c9adc5dea00000", // 1000 eth
	},
	// owner of the governance
	VotingPeriod: 60, // 3 minutes
	// faucet
	Faucet: map[common.Address]string{
		common.HexToAddress("0x00a601f45688dba8a070722073b015277cf36725"): "0x21e19e0c9bab2400000",    // governance
		common.HexToAddress("0xb891fe7b38f857f53a7b5529204c58d5c487280b"): "0x52b7d2dcc80cd2e4000000", // faucet (10kk)
	},
}

var testNetConfig = genesisConfig{
	ChainId: 3332199,
	// who is able to deploy smart contract from genesis block (it won't generate event log)
	Deployers: []common.Address{
		common.HexToAddress("0xEf2AEf8927B2c2c4d9278F97b8c9dae0252dbeD6"),
		common.HexToAddress("0x7f91AB4e20cb5da54A7965F177Dab59624668027"),
		common.HexToAddress("0xA1765cE354E5F3515fB0BBb912ECaC3F04821f57"),
	},
	// list of default validators (it won't generate event log)
	Validators: []common.Address{
		common.HexToAddress("0xc2aCe5085D05732E80e41dFECF26AE0B60E60F04"),
		common.HexToAddress("0x73E46Db39D00a37efEf86621C6a5c33591A00ef5"),
		common.HexToAddress("0xe0579984bD4b3a1F8E1652C84411415A5887d310"),
		common.HexToAddress("0xC72FD6515FeE82e737b34eb8BA9DB4C4A35D47Ac"),
		common.HexToAddress("0x17EBd907EFFD60C83a3450689e1936AfeFaC38Da"),
	},
	SystemTreasury: map[common.Address]uint16{
		common.HexToAddress("0x9C9459Aaf90df6347D4585726F0e97802788f830"): 10000,
	},
	ConsensusParams: consensusParams{
		ActiveValidatorsLength:   13,
		EpochBlockInterval:       1200,                                                                   // (~1hour)
		MisdemeanorThreshold:     100,                                                                    // missed blocks per epoch
		FelonyThreshold:          200,                                                                    // missed blocks per epoch
		ValidatorJailEpochLength: 6,                                                                      // nb of epochs
		UndelegatePeriod:         1,                                                                      // nb of epochs
		MinValidatorStakeAmount:  (*math.HexOrDecimal256)(hexutil.MustDecodeBig("0x3635c9adc5dea00000")), // how many tokens validator must stake to create a validator (in ether)
		MinStakingAmount:         (*math.HexOrDecimal256)(hexutil.MustDecodeBig("0xde0b6b3a7640000")),    // minimum staking amount for delegators (in ether)
	},
	InitialStakes: map[common.Address]string{
		common.HexToAddress("0xc2aCe5085D05732E80e41dFECF26AE0B60E60F04"): "0x152D02C7E14AF6800000", // 100 000 eth
		common.HexToAddress("0x73E46Db39D00a37efEf86621C6a5c33591A00ef5"): "0x3635C9ADC5DEA00000",   // 1000 eth
		common.HexToAddress("0xe0579984bD4b3a1F8E1652C84411415A5887d310"): "0x3635C9ADC5DEA00000",   // 1000 eth
		common.HexToAddress("0xC72FD6515FeE82e737b34eb8BA9DB4C4A35D47Ac"): "0x3635C9ADC5DEA00000",   // 1000 eth
		common.HexToAddress("0x17EBd907EFFD60C83a3450689e1936AfeFaC38Da"): "0x2B5E3AF16B1880000",    // 50 eth
	},
	// owner of the governance
	VotingPeriod: 1200, // (~1hour)
	// faucet
	Faucet: map[common.Address]string{
		common.HexToAddress("0xFc26e7Fe0FeF90e6D9F096EC0847259373402671"): "0x197D7361310E45C669F80000", // faucet 1
	},
	Forks: RTFForks{
		RuntimeUpgradeBlock:    (*math.HexOrDecimal256)(big.NewInt(0)),
		DeployOriginBlock:      (*math.HexOrDecimal256)(big.NewInt(0)),
		DeploymentHookFixBlock: (*math.HexOrDecimal256)(big.NewInt(0)),
	},
}

var spicyConfig = genesisConfig{
	ChainId: 88882,
	// who is able to deploy smart contract from genesis block (it won't generate event log)
	Deployers: []common.Address{
		common.HexToAddress("0x02880217b082cC24D371eB5Bad0827D208bcBC6D"),
	},
	// list of default validators (it won't generate event log)
	Validators: []common.Address{
		common.HexToAddress("0xb1b5a8b8E2a263C0F497BC32a7cb6D27AEA921fc"),
		common.HexToAddress("0x4dD74707f22b74EC872CA6AEB2a065E3d006B9d9"),
		common.HexToAddress("0xBD6D190548bbF5C6920a826dF063A970Bd18f307"),
		common.HexToAddress("0xeC2e502f77c4811f2ef477397235976b1371FCd3"),
		common.HexToAddress("0x1cB3FC9e10fB5b845e53e5EaAE0bD561e662b0A5"),
		common.HexToAddress("0xbdBF08393b66130B4b243863150A265b2A5Df642"),
		common.HexToAddress("0x86f2BB174c450917A1b560c66525E64A1c9B6a04"),
	},
	SystemTreasury: map[common.Address]uint16{
		common.HexToAddress("0x060eA461Cf7E78A38400dE9255687beb9b2c7298"): 10000,
	},
	ConsensusParams: consensusParams{
		ActiveValidatorsLength:   5,
		EpochBlockInterval:       7200,                                                                   // ~6 hours
		MisdemeanorThreshold:     400,                                                                    // missed blocks per epoch
		FelonyThreshold:          800,                                                                    // missed blocks per epoch
		ValidatorJailEpochLength: 4,                                                                      // nb of epochs
		UndelegatePeriod:         1,                                                                      // nb of epochs
		MinValidatorStakeAmount:  (*math.HexOrDecimal256)(hexutil.MustDecodeBig("0x3635c9adc5dea00000")), // how many tokens validator must stake to create a validator (in ether)
		MinStakingAmount:         (*math.HexOrDecimal256)(hexutil.MustDecodeBig("0xde0b6b3a7640000")),    // minimum staking amount for delegators (in ether)
	},
	InitialStakes: map[common.Address]string{
		common.HexToAddress("0xb1b5a8b8E2a263C0F497BC32a7cb6D27AEA921fc"): "0x152D02C7E14AF6800000", // 100 000 CHZ
		common.HexToAddress("0x4dD74707f22b74EC872CA6AEB2a065E3d006B9d9"): "0x3635C9ADC5DEA00000",   // 1000 CHZ
		common.HexToAddress("0xBD6D190548bbF5C6920a826dF063A970Bd18f307"): "0x3635C9ADC5DEA00000",   // 1000 CHZ
		common.HexToAddress("0xeC2e502f77c4811f2ef477397235976b1371FCd3"): "0x3635C9ADC5DEA00000",   // 1000 CHZ
		common.HexToAddress("0x1cB3FC9e10fB5b845e53e5EaAE0bD561e662b0A5"): "0x3635C9ADC5DEA00000",   // 1000 CHZ
		common.HexToAddress("0xbdBF08393b66130B4b243863150A265b2A5Df642"): "0x3635C9ADC5DEA00000",   // 1000 CHZ
		common.HexToAddress("0x86f2BB174c450917A1b560c66525E64A1c9B6a04"): "0x3635C9ADC5DEA00000",   // 1000 CHZ
	},
	VotingPeriod: 1200, // (~1hour)
	// faucet
	Faucet: map[common.Address]string{
		common.HexToAddress("0x77c6DC8fC511Bf2Fa594c47DdC336C69D745e73A"): "0x197D7361310E45C669F80000", // main
		common.HexToAddress("0xa6779032c48127f362244AADD80E3A6E1b50BA93"): "0x33B2E3C9FD0803CE8000000",  // faucet
	},
	Forks: RTFForks{
		RuntimeUpgradeBlock:    (*math.HexOrDecimal256)(big.NewInt(0)),
		DeployOriginBlock:      (*math.HexOrDecimal256)(big.NewInt(0)),
		DeploymentHookFixBlock: (*math.HexOrDecimal256)(big.NewInt(0)),
	},
}

var mainNetConfig = genesisConfig{
	ChainId: 32199,
	// who is able to deploy smart contract from genesis block (it won't generate event log)
	Deployers: []common.Address{
		common.HexToAddress("0xAc3448af2B124d70F5A93aDa08B3EE69c5C9eA0B"),
	},
	// list of default validators (it won't generate event log)
	Validators: []common.Address{
		common.HexToAddress("0xAc3448af2B124d70F5A93aDa08B3EE69c5C9eA0B"),
		common.HexToAddress("0x4fC485Fc2668170033abE0c421F74a5d8CFF4281"),
		common.HexToAddress("0x544EB49544319ee63BC3c7115e45Bf1B3e23c2c2"),
		common.HexToAddress("0x053b4d178AdFA5b8C06d55A7765D6d1486d5c6a0"),
		common.HexToAddress("0xaF3aD38D80E5D4668ddF8CA170Cb941ff5f02244"),
	},
	/**
	 * Here is  share distribution values. (Second parameter in SystemTreasury map)
	 *
	 * Here is some examples:
	 * + 0.3% => 0.3*100=30
	 * + 3% => 3*100=300
	 * + 30% => 30*100=3000
	 * + 100% => 100*100=10000
	 */
	SystemTreasury: map[common.Address]uint16{
		common.HexToAddress("0xFddAc11E0072e3377775345D58de0dc88A964837"): 10000,
	},
	ConsensusParams: consensusParams{
		ActiveValidatorsLength:   5,
		EpochBlockInterval:       300,                                                                       // 15 minutes, if 1 day (28800)
		MisdemeanorThreshold:     14400,                                                                     // missed blocks per epoch
		FelonyThreshold:          21600,                                                                     // missed blocks per epoch
		ValidatorJailEpochLength: 7,                                                                         // nb of epochs
		UndelegatePeriod:         7,                                                                         // nb of epochs
		MinValidatorStakeAmount:  (*math.HexOrDecimal256)(hexutil.MustDecodeBig("0x84595161401484A000000")), // how many tokens validator must stake to create a validator (in ether) - 10,000,000
		MinStakingAmount:         (*math.HexOrDecimal256)(hexutil.MustDecodeBig("0x56BC75E2D63100000")),     // minimum staking amount for delegators (in CHZ) - 100
	},
	VotingPeriod: 271600, // 7 days
	InitialStakes: map[common.Address]string{
		common.HexToAddress("0xAc3448af2B124d70F5A93aDa08B3EE69c5C9eA0B"): "0x84595161401484A000000", // Validator 10,000,000 CHZ
		common.HexToAddress("0x4fC485Fc2668170033abE0c421F74a5d8CFF4281"): "0x84595161401484A000000", // Validator 10,000,000 CHZ
		common.HexToAddress("0x544EB49544319ee63BC3c7115e45Bf1B3e23c2c2"): "0x84595161401484A000000", // Validator 10,000,000 CHZ
		common.HexToAddress("0x053b4d178AdFA5b8C06d55A7765D6d1486d5c6a0"): "0x84595161401484A000000", // Validator 10,000,000 CHZ
		common.HexToAddress("0xaF3aD38D80E5D4668ddF8CA170Cb941ff5f02244"): "0x84595161401484A000000", // Validator 10,000,000 CHZ
	},
	// Supply Distribution
	Faucet: map[common.Address]string{
		common.HexToAddress("0xFddAc11E0072e3377775345D58de0dc88A964837"): "0x1C3CA1E1AAC1A93AF8800000", // Treasury 8,738,880,288 eth
		common.HexToAddress("0xAc3448af2B124d70F5A93aDa08B3EE69c5C9eA0B"): "0x56BC75E2D63100000",        // Validator owner 100 CHZ
		common.HexToAddress("0x4fC485Fc2668170033abE0c421F74a5d8CFF4281"): "0x56BC75E2D63100000",        // Validator owner 100 CHZ
		common.HexToAddress("0x544EB49544319ee63BC3c7115e45Bf1B3e23c2c2"): "0x56BC75E2D63100000",        // Validator owner 100 CHZ
		common.HexToAddress("0x053b4d178AdFA5b8C06d55A7765D6d1486d5c6a0"): "0x56BC75E2D63100000",        // Validator owner 100 CHZ
		common.HexToAddress("0xaF3aD38D80E5D4668ddF8CA170Cb941ff5f02244"): "0x56BC75E2D63100000",        // Validator owner 100 CHZ
		common.HexToAddress("0x252B5CA6c838ae47508c1eA72Dd73b58c607Af0f"): "0x3635C9ADC5DEA00000",       // Bridge relayer 1,000 CHZ
	},
	Forks: RTFForks{
		RuntimeUpgradeBlock:    (*math.HexOrDecimal256)(big.NewInt(0)),
		DeployOriginBlock:      (*math.HexOrDecimal256)(big.NewInt(0)),
		DeploymentHookFixBlock: (*math.HexOrDecimal256)(big.NewInt(0)),
	},
}

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		fileContents, err := os.ReadFile(args[0])
		if err != nil {
			panic(err)
		}
		genesis := &genesisConfig{}
		err = json.Unmarshal(fileContents, genesis)
		if err != nil {
			panic(err)
		}
		outputFile := "stdout"
		if len(args) > 1 {
			outputFile = args[1]
		}
		err = createGenesisConfig(*genesis, outputFile)
		if err != nil {
			panic(err)
		}
		return
	}
	fmt.Printf("building localnet\n")
	if err := createGenesisConfig(localNetConfig, "localnet.json"); err != nil {
		panic(err)
	}
	fmt.Printf("\nbuilding devnet\n")
	if err := createGenesisConfig(devNetConfig, "devnet.json"); err != nil {
		panic(err)
	}
	fmt.Printf("\nbuilding scoville testnet\n")
	if err := createGenesisConfig(testNetConfig, "testnet.json"); err != nil {
		panic(err)
	}
	fmt.Printf("\nbuilding spicy testnet\n")
	if err := createGenesisConfig(spicyConfig, "spicy.json"); err != nil {
		panic(err)
	}
	fmt.Printf("\nbuilding mainnet\n")
	if err := createGenesisConfig(mainNetConfig, "mainnet.json"); err != nil {
		panic(err)
	}
	fmt.Printf("\n")
}
