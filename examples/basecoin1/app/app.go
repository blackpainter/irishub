package app

import (
	"encoding/json"
	"io"
	"os"

	bam "github.com/cosmos/cosmos-sdk/baseapp"
	abci "github.com/tendermint/tendermint/abci/types"
	cmn "github.com/tendermint/tendermint/libs/common"
	dbm "github.com/tendermint/tendermint/libs/db"
	"github.com/tendermint/tendermint/libs/log"
	tmtypes "github.com/tendermint/tendermint/types"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/wire"
	"github.com/cosmos/cosmos-sdk/x/auth"
	"github.com/cosmos/cosmos-sdk/x/bank"
	"github.com/irisnet/irishub/examples/basecoin1/ibc"
	"github.com/cosmos/cosmos-sdk/x/slashing"
	"github.com/cosmos/cosmos-sdk/x/stake"
	"github.com/irisnet/irishub/modules/gov"
	"github.com/irisnet/irishub/modules/upgrade"

	"fmt"
	"github.com/spf13/viper"
	"strings"
)

const (
	appName    = "BasecoinApp"
	FlagReplay = "replay"
)

// default home directories for expected binaries
var (
	DefaultCLIHome  = os.ExpandEnv("$HOME/.basecli")
	DefaultNodeHome = os.ExpandEnv("$HOME/.basecoin")
)

// Extended ABCI application
type BasecoinApp struct {
	*bam.BaseApp
	cdc *wire.Codec

	// keys to access the substores
	keyMain          *sdk.KVStoreKey
	keyAccount       *sdk.KVStoreKey
	keyIBC           *sdk.KVStoreKey
	keyStake         *sdk.KVStoreKey
	keySlashing      *sdk.KVStoreKey
	keyGov           *sdk.KVStoreKey
	keyFeeCollection *sdk.KVStoreKey
	keyUpgrade       *sdk.KVStoreKey

	// Manage getting and setting accounts
	accountMapper       auth.AccountMapper
	feeCollectionKeeper auth.FeeCollectionKeeper
	coinKeeper          bank.Keeper
	ibcMapper           ibc.Mapper
	stakeKeeper         stake.Keeper
	slashingKeeper      slashing.Keeper
	govKeeper           gov.Keeper
	upgradeKeeper       upgrade.Keeper
}

func NewBasecoinApp(logger log.Logger, db dbm.DB, traceStore io.Writer, baseAppOptions ...func(*bam.BaseApp)) *BasecoinApp {
	cdc := MakeCodec()

	bApp := bam.NewBaseApp(appName, cdc, logger, db, baseAppOptions...)
	bApp.SetCommitMultiStoreTracer(traceStore)

	// create your application object
	var app = &BasecoinApp{
		BaseApp:          bam.NewBaseApp(appName, cdc, logger, db),
		cdc:              cdc,
		keyMain:          sdk.NewKVStoreKey("main"),
		keyAccount:       sdk.NewKVStoreKey("acc"),
		keyIBC:           sdk.NewKVStoreKey("ibc"),
		keyStake:         sdk.NewKVStoreKey("stake"),
		keySlashing:      sdk.NewKVStoreKey("slashing"),
		keyGov:           sdk.NewKVStoreKey("gov"),
		keyFeeCollection: sdk.NewKVStoreKey("fee"),
		keyUpgrade:       sdk.NewKVStoreKey("upgrade"),
	}

	var lastHeight int64
	if viper.GetBool(FlagReplay) {
		lastHeight = app.replay()
	}

	// define the accountMapper
	app.accountMapper = auth.NewAccountMapper(
		app.cdc,
		app.keyAccount,        // target store
		auth.ProtoBaseAccount, // prototype
	)

	// add handlers
	app.coinKeeper = bank.NewKeeper(app.accountMapper)
	app.ibcMapper = ibc.NewMapper(app.cdc, app.keyIBC, app.RegisterCodespace(ibc.DefaultCodespace))
	app.stakeKeeper = stake.NewKeeper(app.cdc, app.keyStake, app.coinKeeper, app.RegisterCodespace(stake.DefaultCodespace))
	app.slashingKeeper = slashing.NewKeeper(app.cdc, app.keySlashing, app.stakeKeeper, app.RegisterCodespace(slashing.DefaultCodespace))
	app.govKeeper = gov.NewKeeper(app.cdc, app.keyGov, app.coinKeeper, app.stakeKeeper, app.RegisterCodespace(gov.DefaultCodespace))
	app.feeCollectionKeeper = auth.NewFeeCollectionKeeper(app.cdc, app.keyFeeCollection)
	app.upgradeKeeper = upgrade.NewKeeper(app.cdc, app.keyUpgrade, app.stakeKeeper)

	// register message routes
	app.Router().
		AddRoute("bank", []*sdk.KVStoreKey{app.keyAccount}, bank.NewHandler(app.coinKeeper)).
		AddRoute("ibc", []*sdk.KVStoreKey{app.keyIBC, app.keyAccount}, ibc.NewHandler(app.ibcMapper, app.coinKeeper)).
		AddRoute("stake", []*sdk.KVStoreKey{app.keyStake, app.keyAccount}, stake.NewHandler(app.stakeKeeper)).
		AddRoute("slashing", []*sdk.KVStoreKey{app.keySlashing, app.keyStake}, slashing.NewHandler(app.slashingKeeper)).
		AddRoute("gov", []*sdk.KVStoreKey{app.keyGov, app.keyAccount, app.keyStake}, gov.NewHandler(app.govKeeper)).
		AddRoute("upgrade", []*sdk.KVStoreKey{app.keyUpgrade, app.keyStake}, upgrade.NewHandler(app.upgradeKeeper))

	// initialize BaseApp
	app.SetInitChainer(app.initChainer)
	app.SetBeginBlocker(app.BeginBlocker)
	app.SetEndBlocker(app.EndBlocker)
	app.SetAnteHandler(auth.NewAnteHandler(app.accountMapper, app.feeCollectionKeeper))
	app.MountStoresIAVL(app.keyMain, app.keyAccount, app.keyIBC, app.keyStake, app.keySlashing, app.keyGov, app.keyFeeCollection, app.keyUpgrade)

	var err error
	if viper.GetBool(FlagReplay) {
		err = app.LoadVersion(lastHeight, app.keyMain)
	} else {
		err = app.LoadLatestVersion(app.keyMain)
	}
	if err != nil {
		cmn.Exit(err.Error())
	}

	app.PrepareNewVersion()

	return app
}

// custom tx codec
func MakeCodec() *wire.Codec {
	var cdc = wire.NewCodec()
	ibc.RegisterWire(cdc)
	bank.RegisterWire(cdc)
	stake.RegisterWire(cdc)
	slashing.RegisterWire(cdc)
	gov.RegisterWire(cdc)
	auth.RegisterWire(cdc)
	upgrade.RegisterWire(cdc)
	sdk.RegisterWire(cdc)
	wire.RegisterCrypto(cdc)
	return cdc
}

func (app *BasecoinApp) PrepareNewVersion() {
	store := app.GetKVStore(app.keyUpgrade)
	app.upgradeKeeper.RegisterVersionToBeSwitched(store, app.Router())
}

// application updates every end block
func (app *BasecoinApp) BeginBlocker(ctx sdk.Context, req abci.RequestBeginBlock) abci.ResponseBeginBlock {
	tags := slashing.BeginBlocker(ctx, req, app.slashingKeeper)

	return abci.ResponseBeginBlock{
		Tags: tags.ToKVPairs(),
	}
}

// application updates every end block
func (app *BasecoinApp) EndBlocker(ctx sdk.Context, req abci.RequestEndBlock) abci.ResponseEndBlock {
	validatorUpdates := stake.EndBlocker(ctx, app.stakeKeeper)

	tags, _ := gov.EndBlocker(ctx, app.govKeeper)
	tags.AppendTags(upgrade.EndBlocker(ctx, app.upgradeKeeper))

	return abci.ResponseEndBlock{
		ValidatorUpdates: validatorUpdates,
		Tags:             tags,
	}
}

// custom logic for iris initialization
func (app *BasecoinApp) initChainer(ctx sdk.Context, req abci.RequestInitChain) abci.ResponseInitChain {
	stateJSON := req.AppStateBytes

	var genesisState GenesisState
	err := app.cdc.UnmarshalJSON(stateJSON, &genesisState)
	if err != nil {
		panic(err)
	}

	// load the accounts
	for _, gacc := range genesisState.Accounts {
		acc := gacc.ToAccount()
		acc.AccountNumber = app.accountMapper.GetNextAccountNumber(ctx)
		app.accountMapper.SetAccount(ctx, acc)
	}

	// load the initial stake information
	err = stake.InitGenesis(ctx, app.stakeKeeper, genesisState.StakeData)
	if err != nil {
		panic(err) // TODO https://github.com/cosmos/cosmos-sdk/issues/468
		// return sdk.ErrGenesisParse("").TraceCause(err, "")
	}

	gov.InitGenesis(ctx, app.govKeeper, gov.DefaultGenesisState())

	upgrade.InitGenesis(ctx, app.upgradeKeeper, app.Router())

	return abci.ResponseInitChain{}
}

// export the state of iris for a genesis file
func (app *BasecoinApp) ExportAppStateAndValidators() (appState json.RawMessage, validators []tmtypes.GenesisValidator, err error) {
	ctx := app.NewContext(true, abci.Header{})

	// iterate to get the accounts
	accounts := []GenesisAccount{}
	appendAccount := func(acc auth.Account) (stop bool) {
		account := NewGenesisAccountI(acc)
		accounts = append(accounts, account)
		return false
	}
	app.accountMapper.IterateAccounts(ctx, appendAccount)

	genState := GenesisState{
		Accounts:  accounts,
		StakeData: stake.WriteGenesis(ctx, app.stakeKeeper),
	}
	appState, err = wire.MarshalJSONIndent(app.cdc, genState)
	if err != nil {
		return nil, nil, err
	}
	validators = stake.WriteValidators(ctx, app.stakeKeeper)
	return appState, validators, nil
}

// Iterates through msgs and executes them
func (app *BasecoinApp) runMsgs(ctx sdk.Context, msgs []sdk.Msg) (result sdk.Result) {
	// accumulate results
	logs := make([]string, 0, len(msgs))
	var data []byte   // NOTE: we just append them all (?!)
	var tags sdk.Tags // also just append them all
	var code sdk.ABCICodeType
	for msgIdx, msg := range msgs {
		// Match route.
		msgType, err := app.upgradeKeeper.GetMsgTypeInCurrentVersion(ctx, msg)
		if err != nil {
			return err.Result()
		}

		handler := app.Router().Route(msgType)
		if handler == nil {
			return sdk.ErrUnknownRequest("Unrecognized Msg type: " + msgType).Result()
		}

		msgResult := handler(ctx, msg)

		// NOTE: GasWanted is determined by ante handler and
		// GasUsed by the GasMeter

		// Append Data and Tags
		data = append(data, msgResult.Data...)
		tags = append(tags, msgResult.Tags...)

		// Stop execution and return on first failed message.
		if !msgResult.IsOK() {
			logs = append(logs, fmt.Sprintf("Msg %d failed: %s", msgIdx, msgResult.Log))
			code = msgResult.Code
			break
		}

		// Construct usable logs in multi-message transactions.
		logs = append(logs, fmt.Sprintf("Msg %d: %s", msgIdx, msgResult.Log))
	}

	// Set the final gas values.
	result = sdk.Result{
		Code:    code,
		Data:    data,
		Log:     strings.Join(logs, "\n"),
		GasUsed: ctx.GasMeter().GasConsumed(),
		// TODO: FeeAmount/FeeDenom
		Tags: tags,
	}

	return result
}

func (app *BasecoinApp) replay() int64 {
	//ctx := server.NewDefaultContext()
	//ctx.Config.RootDir = viper.GetString(tmcli.HomeFlag)
	//dbContext := node.DBContext{"state", ctx.Config}
	//dbType := dbm.DBBackendType(dbContext.Config.DBBackend)
	//stateDB := dbm.NewDB(dbContext.ID, dbType, dbContext.Config.DBDir())
	//
	//preState := sm.LoadPreState(stateDB)
	//if preState.LastBlockHeight == 0 {
	//	panic(errors.New("can't replay the last block, last block height is 0"))
	//}
	//
	//sm.SaveState(stateDB,preState)
	//stateDB.Close()
	//
	//return preState.LastBlockHeight

	return 0
}