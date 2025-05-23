package keeper_test

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	"testing"

	cosmoslog "cosmossdk.io/log"
	cosmosmath "cosmossdk.io/math"
	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	govtypes "github.com/cosmos/cosmos-sdk/x/gov/types"
	"github.com/pokt-network/smt"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/poktroll/app/volatile"
	"github.com/pokt-network/poktroll/cmd/pocketd/cmd"
	"github.com/pokt-network/poktroll/pkg/crypto/protocol"
	"github.com/pokt-network/poktroll/pkg/encoding"
	testutilevents "github.com/pokt-network/poktroll/testutil/events"
	testkeeper "github.com/pokt-network/poktroll/testutil/keeper"
	testproof "github.com/pokt-network/poktroll/testutil/proof"
	"github.com/pokt-network/poktroll/testutil/sample"
	testsession "github.com/pokt-network/poktroll/testutil/session"
	sharedtest "github.com/pokt-network/poktroll/testutil/shared"
	apptypes "github.com/pokt-network/poktroll/x/application/types"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	suppliertypes "github.com/pokt-network/poktroll/x/supplier/types"
	tokenomicskeeper "github.com/pokt-network/poktroll/x/tokenomics/keeper"
	tlm "github.com/pokt-network/poktroll/x/tokenomics/token_logic_module"
	tokenomicstypes "github.com/pokt-network/poktroll/x/tokenomics/types"
)

func init() {
	cmd.InitSDKConfig()
}

// TODO_IMPROVE: Consider using a TestSuite, similar to `x/tokenomics/keeper/keeper_settle_pending_claims_test.go`
// for the TLM based tests in this file.

func TestProcessTokenLogicModules_TLMBurnEqualsMint_Valid(t *testing.T) {
	// Test Parameters
	appInitialStake := apptypes.DefaultMinStake.Amount.Mul(cosmosmath.NewInt(2))
	supplierInitialStake := cosmosmath.NewInt(1000000)
	supplierRevShareRatios := []uint64{12, 38, 50}
	globalComputeUnitsToTokensMultiplier := uint64(1)
	serviceComputeUnitsPerRelay := uint64(1)
	service := prepareTestService(serviceComputeUnitsPerRelay)
	numRelays := uint64(1000) // By supplier for application in this session

	// Prepare the keepers
	keepers, ctx := testkeeper.NewTokenomicsModuleKeepers(t,
		cosmoslog.NewNopLogger(),
		testkeeper.WithService(*service),
		testkeeper.WithDefaultModuleBalances(),
	)
	ctx = sdk.UnwrapSDKContext(ctx).WithBlockHeight(1)
	keepers.SetService(ctx, *service)

	// Ensure the claim is within relay mining bounds
	numSuppliersPerSession := int64(keepers.SessionKeeper.GetParams(ctx).NumSuppliersPerSession)
	numTokensClaimed := int64(numRelays * serviceComputeUnitsPerRelay * globalComputeUnitsToTokensMultiplier)
	maxClaimableAmountPerSupplier := appInitialStake.Quo(cosmosmath.NewInt(numSuppliersPerSession))
	require.GreaterOrEqual(t, maxClaimableAmountPerSupplier.Int64(), numTokensClaimed)

	// Retrieve the app and supplier module addresses
	appModuleAddress := authtypes.NewModuleAddress(apptypes.ModuleName).String()
	supplierModuleAddress := authtypes.NewModuleAddress(suppliertypes.ModuleName).String()

	// Set compute_units_to_tokens_multiplier to simplify expectation calculations.
	sharedParams := keepers.SharedKeeper.GetParams(ctx)
	sharedParams.ComputeUnitsToTokensMultiplier = globalComputeUnitsToTokensMultiplier
	err := keepers.SharedKeeper.SetParams(ctx, sharedParams)
	require.NoError(t, err)
	// TODO_TECHDEBT: Setting inflation to zero so we are testing the BurnEqualsMint logic exclusively.
	// Once it is a governance param, update it using the keeper above.
	tokenomicsParams := keepers.Keeper.GetParams(ctx)
	tokenomicsParams.GlobalInflationPerClaim = 0
	err = keepers.Keeper.SetParams(ctx, tokenomicsParams)
	require.NoError(t, err)

	// Add a new application with non-zero app stake end balance to assert against.
	appStake := cosmostypes.NewCoin(volatile.DenomuPOKT, appInitialStake)
	app := apptypes.Application{
		Address:        sample.AccAddress(),
		Stake:          &appStake,
		ServiceConfigs: []*sharedtypes.ApplicationServiceConfig{{ServiceId: service.Id}},
	}
	keepers.SetApplication(ctx, app)

	// Prepare the supplier revenue shares
	supplierRevShares := make([]*sharedtypes.ServiceRevenueShare, len(supplierRevShareRatios))
	for i := range supplierRevShares {
		shareHolderAddress := sample.AccAddress()
		supplierRevShares[i] = &sharedtypes.ServiceRevenueShare{
			Address:            shareHolderAddress,
			RevSharePercentage: supplierRevShareRatios[i],
		}
	}
	services := []*sharedtypes.SupplierServiceConfig{{
		ServiceId: service.Id,
		RevShare:  supplierRevShares,
	}}

	// Add a new supplier.
	supplierStake := cosmostypes.NewCoin(volatile.DenomuPOKT, supplierInitialStake)
	serviceConfigHistory := sharedtest.CreateServiceConfigUpdateHistoryFromServiceConfigs(
		supplierRevShares[0].Address,
		services, 1, 0,
	)
	supplier := sharedtypes.Supplier{
		// Make the first shareholder the supplier itself.
		OwnerAddress:         supplierRevShares[0].Address,
		OperatorAddress:      supplierRevShares[0].Address,
		Stake:                &supplierStake,
		Services:             services,
		ServiceConfigHistory: serviceConfigHistory,
	}
	keepers.SetAndIndexDehydratedSupplier(ctx, supplier)

	// Query the account and module start balances
	appStartBalance := getBalance(t, ctx, keepers, app.GetAddress())
	appModuleStartBalance := getBalance(t, ctx, keepers, appModuleAddress)
	supplierModuleStartBalance := getBalance(t, ctx, keepers, supplierModuleAddress)

	// Prepare the claim for which the supplier did work for the application
	claim := prepareTestClaim(numRelays, service, &app, &supplier)
	pendingResult := tlm.NewClaimSettlementResult(claim)

	settlementContext := tokenomicskeeper.NewSettlementContext(
		ctx,
		keepers.Keeper,
		keepers.Keeper.Logger(),
	)

	err = settlementContext.ClaimCacheWarmUp(ctx, &claim)
	require.NoError(t, err)

	// Process the token logic modules
	err = keepers.ProcessTokenLogicModules(ctx, settlementContext, pendingResult)
	require.NoError(t, err)

	// Execute the pending results
	pendingResults := make(tlm.ClaimSettlementResults, 0)
	pendingResults.Append(pendingResult)
	err = keepers.ExecutePendingSettledResults(cosmostypes.UnwrapSDKContext(ctx), pendingResults)
	require.NoError(t, err)

	// Persist the actors state
	settlementContext.FlushAllActorsToStore(ctx)

	// Assert that `applicationAddress` account balance is *unchanged*
	appEndBalance := getBalance(t, ctx, keepers, app.GetAddress())
	require.EqualValues(t, appStartBalance, appEndBalance)

	// Determine the expected app end stake amount and the expected app burn
	appBurn := cosmosmath.NewInt(numTokensClaimed)
	expectedAppEndStakeAmount := appInitialStake.Sub(appBurn)

	// Assert that `applicationAddress` staked balance has decreased by the appropriate amount
	app, appIsFound := keepers.GetApplication(ctx, app.GetAddress())
	actualAppEndStakeAmount := app.GetStake().Amount
	require.True(t, appIsFound)
	require.Equal(t, expectedAppEndStakeAmount, actualAppEndStakeAmount)

	// Assert that app module balance is *decreased* by the appropriate amount
	// NB: The application module account burns the amount of uPOKT that was held in escrow
	// on behalf of the applications which were serviced in a given session.
	expectedAppModuleEndBalance := appModuleStartBalance.Sub(sdk.NewCoin(volatile.DenomuPOKT, appBurn))
	appModuleEndBalance := getBalance(t, ctx, keepers, appModuleAddress)
	require.NotNil(t, appModuleEndBalance)
	require.EqualValues(t, &expectedAppModuleEndBalance, appModuleEndBalance)

	// Assert that `supplierOperatorAddress` staked balance is *unchanged*
	supplier, supplierIsFound := keepers.GetSupplier(ctx, supplier.GetOperatorAddress())
	require.True(t, supplierIsFound)
	require.Equal(t, &supplierStake, supplier.GetStake())

	// Assert that `suppliertypes.ModuleName` account module balance is *unchanged*
	// NB: Supplier rewards are minted to the supplier module account but then immediately
	// distributed to the supplier accounts which provided service in a given session.
	supplierModuleEndBalance := getBalance(t, ctx, keepers, supplierModuleAddress)
	require.EqualValues(t, supplierModuleStartBalance, supplierModuleEndBalance)

	// Assert that the supplier shareholders account balances have *increased* by
	// the appropriate amount w.r.t token distribution.
	shareAmounts := tlm.GetShareAmountMap(supplierRevShares, appBurn)
	for shareHolderAddr, expectedShareAmount := range shareAmounts {
		shareHolderBalance := getBalance(t, ctx, keepers, shareHolderAddr)
		require.Equal(t, expectedShareAmount, shareHolderBalance.Amount)
	}
}

// DEV_NOTE: Most of the setup here is a copy-paste of TLMBurnEqualsMintValid
// except that the application stake is calculated to explicitly be too low to
// handle all the relays completed.
func TestProcessTokenLogicModules_TLMBurnEqualsMint_Valid_SupplierExceedsMaxClaimableAmount(t *testing.T) {
	// Test Parameters
	globalComputeUnitsToTokensMultiplier := uint64(1)
	serviceComputeUnitsPerRelay := uint64(100)
	service := prepareTestService(serviceComputeUnitsPerRelay)
	numRelays := uint64(1000) // By a single supplier for application in this session
	supplierInitialStake := cosmosmath.NewInt(1000000)
	supplierRevShareRatios := []uint64{12, 38, 50}

	// Prepare the keepers
	keepers, ctx := testkeeper.NewTokenomicsModuleKeepers(t,
		cosmoslog.NewNopLogger(),
		testkeeper.WithService(*service),
		testkeeper.WithDefaultModuleBalances(),
	)
	ctx = sdk.UnwrapSDKContext(ctx).WithBlockHeight(1)
	keepers.SetService(ctx, *service)

	// Set up the relays to exceed the max claimable amount
	// Determine the max a supplier can claim
	maxClaimableAmountPerSupplier := int64(numRelays * serviceComputeUnitsPerRelay * globalComputeUnitsToTokensMultiplier)
	// Figure out what the app's initial stake should be to cover the max claimable amount
	numSuppliersPerSession := int64(keepers.SessionKeeper.GetParams(ctx).NumSuppliersPerSession)
	appInitialStake := cosmosmath.NewInt(maxClaimableAmountPerSupplier*numSuppliersPerSession + 1)
	// Increase the number of relay such that the supplier did "free work" and would
	// be able to claim more than the max claimable amount.
	numRelays *= 5
	numTokensClaimed := int64(numRelays * serviceComputeUnitsPerRelay * globalComputeUnitsToTokensMultiplier)

	// Retrieve the app and supplier module addresses
	appModuleAddress := authtypes.NewModuleAddress(apptypes.ModuleName).String()
	supplierModuleAddress := authtypes.NewModuleAddress(suppliertypes.ModuleName).String()

	// Set compute_units_to_tokens_multiplier to simplify expectation calculations.
	sharedParams := keepers.SharedKeeper.GetParams(ctx)
	sharedParams.ComputeUnitsToTokensMultiplier = globalComputeUnitsToTokensMultiplier
	err := keepers.SharedKeeper.SetParams(ctx, sharedParams)
	require.NoError(t, err)
	// TODO_TECHDEBT: Setting inflation to zero so we are testing the BurnEqualsMint logic exclusively.
	// Once it is a governance param, update it using the keeper above.
	tokenomicsParams := keepers.Keeper.GetParams(ctx)
	tokenomicsParams.GlobalInflationPerClaim = 0
	err = keepers.Keeper.SetParams(ctx, tokenomicsParams)
	require.NoError(t, err)

	// Add a new application with non-zero app stake end balance to assert against.
	appStake := cosmostypes.NewCoin(volatile.DenomuPOKT, appInitialStake)
	app := apptypes.Application{
		Address:        sample.AccAddress(),
		Stake:          &appStake,
		ServiceConfigs: []*sharedtypes.ApplicationServiceConfig{{ServiceId: service.Id}},
	}
	keepers.SetApplication(ctx, app)

	// Prepare the supplier revenue shares
	supplierRevShares := make([]*sharedtypes.ServiceRevenueShare, len(supplierRevShareRatios))
	for i := range supplierRevShares {
		shareHolderAddress := sample.AccAddress()
		supplierRevShares[i] = &sharedtypes.ServiceRevenueShare{
			Address:            shareHolderAddress,
			RevSharePercentage: supplierRevShareRatios[i],
		}
	}
	services := []*sharedtypes.SupplierServiceConfig{{
		ServiceId: service.Id,
		RevShare:  supplierRevShares,
	}}

	// Add a new supplier.
	supplierStake := cosmostypes.NewCoin(volatile.DenomuPOKT, supplierInitialStake)
	serviceConfigHistory := sharedtest.CreateServiceConfigUpdateHistoryFromServiceConfigs(
		supplierRevShares[0].Address,
		services, 1, 0,
	)
	supplier := sharedtypes.Supplier{
		// Make the first shareholder the supplier itself.
		OwnerAddress:         supplierRevShares[0].Address,
		OperatorAddress:      supplierRevShares[0].Address,
		Stake:                &supplierStake,
		Services:             services,
		ServiceConfigHistory: serviceConfigHistory,
	}
	keepers.SetAndIndexDehydratedSupplier(ctx, supplier)

	// Query the account and module start balances
	appStartBalance := getBalance(t, ctx, keepers, app.GetAddress())
	appModuleStartBalance := getBalance(t, ctx, keepers, appModuleAddress)
	supplierModuleStartBalance := getBalance(t, ctx, keepers, supplierModuleAddress)

	// Prepare the claim for which the supplier did work for the application
	claim := prepareTestClaim(numRelays, service, &app, &supplier)
	pendingResult := tlm.NewClaimSettlementResult(claim)

	settlementContext := tokenomicskeeper.NewSettlementContext(
		ctx,
		keepers.Keeper,
		keepers.Keeper.Logger(),
	)

	err = settlementContext.ClaimCacheWarmUp(ctx, &claim)
	require.NoError(t, err)

	// Process the token logic modules
	err = keepers.ProcessTokenLogicModules(ctx, settlementContext, pendingResult)
	require.NoError(t, err)

	// Execute the pending results
	pendingResults := make(tlm.ClaimSettlementResults, 0)
	pendingResults.Append(pendingResult)
	err = keepers.ExecutePendingSettledResults(cosmostypes.UnwrapSDKContext(ctx), pendingResults)
	require.NoError(t, err)

	// Persist the actors state
	settlementContext.FlushAllActorsToStore(ctx)

	// Assert that `applicationAddress` account balance is *unchanged*
	appEndBalance := getBalance(t, ctx, keepers, app.GetAddress())
	require.EqualValues(t, appStartBalance, appEndBalance)

	// Determine the expected app end stake amount and the expected app burn
	appBurn := cosmosmath.NewInt(maxClaimableAmountPerSupplier)
	appBurnCoin := sdk.NewCoin(volatile.DenomuPOKT, appBurn)
	expectedAppEndStakeAmount := appInitialStake.Sub(appBurn)

	// Assert that `applicationAddress` staked balance has decreased by the max claimable amount
	app, appIsFound := keepers.GetApplication(ctx, app.GetAddress())
	actualAppEndStakeAmount := app.GetStake().Amount
	require.True(t, appIsFound)
	require.Equal(t, expectedAppEndStakeAmount, actualAppEndStakeAmount)

	// Sanity
	require.Less(t, maxClaimableAmountPerSupplier, numTokensClaimed)

	// Assert that app module balance is *decreased* by the appropriate amount
	// NB: The application module account burns the amount of uPOKT that was held in escrow
	// on behalf of the applications which were serviced in a given session.
	expectedAppModuleEndBalance := appModuleStartBalance.Sub(appBurnCoin)
	appModuleEndBalance := getBalance(t, ctx, keepers, appModuleAddress)
	require.NotNil(t, appModuleEndBalance)
	require.EqualValues(t, &expectedAppModuleEndBalance, appModuleEndBalance)

	// Assert that `supplierOperatorAddress` staked balance is *unchanged*
	supplier, supplierIsFound := keepers.GetSupplier(ctx, supplier.GetOperatorAddress())
	require.True(t, supplierIsFound)
	require.Equal(t, &supplierStake, supplier.GetStake())

	// Assert that `suppliertypes.ModuleName` account module balance is *unchanged*
	// NB: Supplier rewards are minted to the supplier module account but then immediately
	// distributed to the supplier accounts which provided service in a given session.
	supplierModuleEndBalance := getBalance(t, ctx, keepers, supplierModuleAddress)
	require.EqualValues(t, supplierModuleStartBalance, supplierModuleEndBalance)

	// Assert that the supplier shareholders account balances have *increased* by
	// the appropriate amount w.r.t token distribution.
	shareAmounts := tlm.GetShareAmountMap(supplierRevShares, appBurn)
	for shareHolderAddr, expectedShareAmount := range shareAmounts {
		shareHolderBalance := getBalance(t, ctx, keepers, shareHolderAddr)
		require.Equal(t, expectedShareAmount, shareHolderBalance.Amount)
	}

	// Check that the expected burn >> effective burn because application is overserviced

	sdkCtx := sdk.UnwrapSDKContext(ctx)
	events := sdkCtx.EventManager().Events()
	appOverservicedEvents := testutilevents.FilterEvents[*tokenomicstypes.EventApplicationOverserviced](t, events)
	require.Len(t, appOverservicedEvents, 1, "unexpected number of event overserviced events")
	appOverservicedEvent := appOverservicedEvents[0]

	require.Equal(t, app.GetAddress(), appOverservicedEvent.ApplicationAddr)
	require.Equal(t, supplier.GetOperatorAddress(), appOverservicedEvent.SupplierOperatorAddr)
	require.Equal(t, numTokensClaimed, appOverservicedEvent.ExpectedBurn.Amount.Int64())
	require.Equal(t, appBurn, appOverservicedEvent.EffectiveBurn.Amount)
	require.Less(t, appBurn.Int64(), numTokensClaimed)
}

func TestProcessTokenLogicModules_TLMGlobalMint_Valid_MintDistributionCorrect(t *testing.T) {
	// Test Parameters
	appInitialStake := apptypes.DefaultMinStake.Amount.Mul(cosmosmath.NewInt(2))
	supplierInitialStake := cosmosmath.NewInt(1000000)
	supplierRevShareRatios := []uint64{12, 38, 50}
	globalComputeUnitsToTokensMultiplier := uint64(1)
	serviceComputeUnitsPerRelay := uint64(1)
	service := prepareTestService(serviceComputeUnitsPerRelay)
	numRelays := uint64(1000) // By supplier for application in this session
	numTokensClaimed := numRelays * serviceComputeUnitsPerRelay * globalComputeUnitsToTokensMultiplier
	numTokensClaimedInt := cosmosmath.NewIntFromUint64(numTokensClaimed)
	proposerConsAddr := sample.ConsAddressBech32()
	daoAddress := authtypes.NewModuleAddress(govtypes.ModuleName)

	tokenLogicModules := tlm.NewDefaultTokenLogicModules()

	// Prepare the keepers
	opts := []testkeeper.TokenomicsModuleKeepersOptFn{
		testkeeper.WithService(*service),
		testkeeper.WithProposerAddr(proposerConsAddr),
		testkeeper.WithTokenLogicModules(tokenLogicModules),
		testkeeper.WithDefaultModuleBalances(),
	}
	keepers, ctx := testkeeper.NewTokenomicsModuleKeepers(t, nil, opts...)
	ctx = sdk.UnwrapSDKContext(ctx).WithBlockHeight(1)
	keepers.SetService(ctx, *service)

	// Set the dao_reward_address param on the tokenomics keeper.
	tokenomicsParams := keepers.Keeper.GetParams(ctx)
	tokenomicsParams.DaoRewardAddress = daoAddress.String()
	keepers.Keeper.SetParams(ctx, tokenomicsParams)

	// Set compute_units_to_tokens_multiplier to simplify expectation calculations.
	sharedParams := keepers.SharedKeeper.GetParams(ctx)
	sharedParams.ComputeUnitsToTokensMultiplier = globalComputeUnitsToTokensMultiplier
	err := keepers.SharedKeeper.SetParams(ctx, sharedParams)
	require.NoError(t, err)

	// Add a new application with non-zero app stake end balance to assert against.
	appStake := cosmostypes.NewCoin(volatile.DenomuPOKT, appInitialStake)
	app := apptypes.Application{
		Address:        sample.AccAddress(),
		Stake:          &appStake,
		ServiceConfigs: []*sharedtypes.ApplicationServiceConfig{{ServiceId: service.Id}},
	}
	keepers.SetApplication(ctx, app)

	// Prepare the supplier revenue shares
	supplierRevShares := make([]*sharedtypes.ServiceRevenueShare, len(supplierRevShareRatios))
	for i := range supplierRevShares {
		shareHolderAddress := sample.AccAddress()
		supplierRevShares[i] = &sharedtypes.ServiceRevenueShare{
			Address:            shareHolderAddress,
			RevSharePercentage: supplierRevShareRatios[i],
		}
	}
	services := []*sharedtypes.SupplierServiceConfig{{ServiceId: service.Id, RevShare: supplierRevShares}}

	// Add a new supplier.
	supplierStake := cosmostypes.NewCoin(volatile.DenomuPOKT, supplierInitialStake)
	serviceConfigHistory := sharedtest.CreateServiceConfigUpdateHistoryFromServiceConfigs(
		supplierRevShares[0].Address,
		services, 1, 0,
	)
	supplier := sharedtypes.Supplier{
		// Make the first shareholder the supplier itself.
		OwnerAddress:         supplierRevShares[0].Address,
		OperatorAddress:      supplierRevShares[0].Address,
		Stake:                &supplierStake,
		Services:             services,
		ServiceConfigHistory: serviceConfigHistory,
	}
	keepers.SetAndIndexDehydratedSupplier(ctx, supplier)

	// Prepare the claim for which the supplier did work for the application
	claim := prepareTestClaim(numRelays, service, &app, &supplier)
	pendingResult := tlm.NewClaimSettlementResult(claim)

	// Prepare addresses
	appAddress := app.Address
	proposerAddress := sample.AccAddressFromConsBech32(proposerConsAddr)

	// Determine balances before inflation
	daoBalanceBefore := getBalance(t, ctx, keepers, daoAddress.String())
	propBalanceBefore := getBalance(t, ctx, keepers, proposerAddress)
	serviceOwnerBalanceBefore := getBalance(t, ctx, keepers, service.OwnerAddress)
	appBalanceBefore := getBalance(t, ctx, keepers, appAddress)
	supplierShareholderBalancesBeforeSettlementMap := make(map[string]*sdk.Coin, len(supplierRevShares))
	for _, revShare := range supplierRevShares {
		addr := revShare.Address
		supplierShareholderBalancesBeforeSettlementMap[addr] = getBalance(t, ctx, keepers, addr)
	}

	settlementContext := tokenomicskeeper.NewSettlementContext(
		ctx,
		keepers.Keeper,
		keepers.Keeper.Logger(),
	)

	err = settlementContext.ClaimCacheWarmUp(ctx, &claim)
	require.NoError(t, err)

	// Process the token logic modules
	err = keepers.ProcessTokenLogicModules(ctx, settlementContext, pendingResult)
	require.NoError(t, err)

	// Persist the actors state
	settlementContext.FlushAllActorsToStore(ctx)

	// Execute the pending results
	pendingResults := make(tlm.ClaimSettlementResults, 0)
	pendingResults.Append(pendingResult)
	err = keepers.ExecutePendingSettledResults(cosmostypes.UnwrapSDKContext(ctx), pendingResults)
	require.NoError(t, err)

	// Determine balances after inflation
	daoBalanceAfter := getBalance(t, ctx, keepers, daoAddress.String())
	propBalanceAfter := getBalance(t, ctx, keepers, proposerAddress)
	serviceOwnerBalanceAfter := getBalance(t, ctx, keepers, service.OwnerAddress)
	appBalanceAfter := getBalance(t, ctx, keepers, appAddress)
	supplierShareholderBalancesAfter := make(map[string]*sdk.Coin, len(supplierRevShares))
	for _, revShare := range supplierRevShares {
		addr := revShare.Address
		supplierShareholderBalancesAfter[addr] = getBalance(t, ctx, keepers, addr)
	}

	// Compute the expected amount to mint.
	globalInflationPerClaimRat, err := encoding.Float64ToRat(tokenomicsParams.GlobalInflationPerClaim)
	require.NoError(t, err)

	numTokensClaimedRat := new(big.Rat).SetInt(numTokensClaimedInt.BigInt())
	numTokensMintedRat := new(big.Rat).Mul(numTokensClaimedRat, globalInflationPerClaimRat)
	reminder := new(big.Int)
	numTokensMintedInt, reminder := new(big.Int).QuoRem(
		numTokensMintedRat.Num(),
		numTokensMintedRat.Denom(),
		reminder,
	)

	// Ceil the number of tokens minted if there is a remainder.
	if reminder.Cmp(big.NewInt(0)) != 0 {
		numTokensMintedInt = numTokensMintedInt.Add(numTokensMintedInt, big.NewInt(1))
	}
	numTokensMinted := cosmosmath.NewIntFromBigInt(numTokensMintedInt)

	// Compute the expected amount minted to each module.
	propMint := computeShare(t, numTokensMintedRat, tokenomicsParams.MintAllocationPercentages.Proposer)
	serviceOwnerMint := computeShare(t, numTokensMintedRat, tokenomicsParams.MintAllocationPercentages.SourceOwner)
	appMint := computeShare(t, numTokensMintedRat, tokenomicsParams.MintAllocationPercentages.Application)
	supplierMint := computeShare(t, numTokensMintedRat, tokenomicsParams.MintAllocationPercentages.Supplier)
	// The DAO mint gets any remainder resulting from integer division.
	daoMint := numTokensMinted.Sub(propMint).Sub(serviceOwnerMint).Sub(appMint).Sub(supplierMint)

	// Ensure the balance was increased to the appropriate amount.
	require.Equal(t, propBalanceBefore.Amount.Add(propMint), propBalanceAfter.Amount)
	require.Equal(t, serviceOwnerBalanceBefore.Amount.Add(serviceOwnerMint), serviceOwnerBalanceAfter.Amount)
	require.Equal(t, appBalanceBefore.Amount.Add(appMint), appBalanceAfter.Amount)
	require.Equal(t, daoBalanceBefore.Amount.Add(daoMint).Add(numTokensMinted), daoBalanceAfter.Amount)

	supplierMintRat := new(big.Rat).SetInt(supplierMint.BigInt())
	shareHoldersBalancesAfterSettlementMap := make(map[string]cosmosmath.Int, len(supplierRevShares))
	supplierMintWithoutRemainder := cosmosmath.NewInt(0)
	for _, revShare := range supplierRevShares {
		addr := revShare.Address

		// Compute the expected balance increase for the shareholder
		mintShareFloat := float64(revShare.RevSharePercentage) / 100.0
		rewardShare := computeShare(t, numTokensClaimedRat, mintShareFloat)
		mintShare := computeShare(t, supplierMintRat, mintShareFloat)
		balanceIncrease := rewardShare.Add(mintShare)

		// Compute the expected balance after minting
		balanceBefore := supplierShareholderBalancesBeforeSettlementMap[addr]
		shareHoldersBalancesAfterSettlementMap[addr] = balanceBefore.Amount.Add(balanceIncrease)

		supplierMintWithoutRemainder = supplierMintWithoutRemainder.Add(mintShare)
	}

	// The first shareholder gets any remainder resulting from integer division.
	firstShareHolderAddr := supplierRevShares[0].Address
	firstShareHolderBalance := shareHoldersBalancesAfterSettlementMap[firstShareHolderAddr]
	remainder := supplierMint.Sub(supplierMintWithoutRemainder)
	shareHoldersBalancesAfterSettlementMap[firstShareHolderAddr] = firstShareHolderBalance.Add(remainder)

	for _, revShare := range supplierRevShares {
		addr := revShare.Address
		balanceAfter := supplierShareholderBalancesAfter[addr].Amount
		expectedBalanceAfter := shareHoldersBalancesAfterSettlementMap[addr]
		require.Equal(t, expectedBalanceAfter, balanceAfter)
	}

	foundApp, appFound := keepers.GetApplication(ctx, appAddress)
	require.True(t, appFound)

	appStakeAfter := foundApp.GetStake().Amount
	expectedStakeAfter := appInitialStake.Sub(numTokensMinted).Sub(numTokensClaimedInt)
	require.Equal(t, expectedStakeAfter, appStakeAfter)
}

func TestProcessTokenLogicModules_AppNotFound(t *testing.T) {
	keeper, ctx, _, supplierOperatorAddr, service := testkeeper.TokenomicsKeeperWithActorAddrs(t)

	// The base claim whose root will be customized for testing purposes
	numRelays := uint64(42)
	numComputeUnits := numRelays * service.ComputeUnitsPerRelay
	claim := prooftypes.Claim{
		SupplierOperatorAddress: supplierOperatorAddr,
		SessionHeader: &sessiontypes.SessionHeader{
			ApplicationAddress:      sample.AccAddress(), // Random address
			ServiceId:               service.Id,
			SessionId:               "session_id",
			SessionStartBlockHeight: 1,
			SessionEndBlockHeight:   testsession.GetSessionEndHeightWithDefaultParams(1),
		},
		RootHash: testproof.SmstRootWithSumAndCount(numComputeUnits, numRelays),
	}
	pendingResult := tlm.NewClaimSettlementResult(claim)

	settlementContext := tokenomicskeeper.NewSettlementContext(ctx, &keeper, keeper.Logger())

	// Ignoring the error from ClaimCacheWarmUp as it will short-circuit the test
	// and we want to test the error from ProcessTokenLogicModules.
	_ = settlementContext.ClaimCacheWarmUp(ctx, &claim)

	// Process the token logic modules
	err := keeper.ProcessTokenLogicModules(ctx, settlementContext, pendingResult)
	require.Error(t, err)
	require.ErrorIs(t, err, tokenomicstypes.ErrTokenomicsApplicationNotFound)
}

func TestProcessTokenLogicModules_ServiceNotFound(t *testing.T) {
	keeper, ctx, appAddr, supplierOperatorAddr, service := testkeeper.TokenomicsKeeperWithActorAddrs(t)

	numRelays := uint64(42)
	numComputeUnits := numRelays * service.ComputeUnitsPerRelay
	claim := prooftypes.Claim{
		SupplierOperatorAddress: supplierOperatorAddr,
		SessionHeader: &sessiontypes.SessionHeader{
			ApplicationAddress:      appAddr,
			ServiceId:               "non_existent_svc",
			SessionId:               "session_id",
			SessionStartBlockHeight: 1,
			SessionEndBlockHeight:   testsession.GetSessionEndHeightWithDefaultParams(1),
		},
		RootHash: testproof.SmstRootWithSumAndCount(numComputeUnits, numRelays),
	}
	pendingResult := tlm.NewClaimSettlementResult(claim)

	settlementContext := tokenomicskeeper.NewSettlementContext(ctx, &keeper, keeper.Logger())

	// Ignoring the error from ClaimCacheWarmUp as it will short-circuit the test
	// and we want to test the error from ProcessTokenLogicModules.
	_ = settlementContext.ClaimCacheWarmUp(ctx, &claim)

	// Execute test function
	err := keeper.ProcessTokenLogicModules(ctx, settlementContext, pendingResult)
	require.Error(t, err)
	require.ErrorIs(t, err, tokenomicstypes.ErrTokenomicsServiceNotFound)
}

func TestProcessTokenLogicModules_InvalidRoot(t *testing.T) {
	keeper, ctx, appAddr, supplierOperatorAddr, service := testkeeper.TokenomicsKeeperWithActorAddrs(t)
	numRelays := uint64(42)

	// Define test cases
	tests := []struct {
		desc        string
		root        []byte // smst.MerkleSumRoot
		errExpected bool
	}{
		{
			desc:        "Nil Root",
			root:        nil,
			errExpected: true,
		},
		{
			desc:        fmt.Sprintf("Less than %d bytes", protocol.TrieRootSize),
			root:        make([]byte, protocol.TrieRootSize-1), // Less than expected number of bytes
			errExpected: true,
		},
		{
			desc:        fmt.Sprintf("More than %d bytes", protocol.TrieRootSize),
			root:        make([]byte, protocol.TrieRootSize+1), // More than expected number of bytes
			errExpected: true,
		},
		{
			desc: "correct size but empty",
			root: func() []byte {
				root := make([]byte, protocol.TrieRootSize) // All 0s
				return root[:]
			}(),
			errExpected: true,
		},
		{
			desc: "correct size but invalid value",
			root: func() []byte {
				// A root with all 'a's is a valid value since each of the hash, sum and size
				// will be []byte{0x61, 0x61, ...} with their respective sizes.
				// The current test suite sets the CUPR to 1, making sum == count * CUPR
				// valid. So, we can change the last byte to 'b' to make it invalid.
				root := bytes.Repeat([]byte("a"), protocol.TrieRootSize)
				root = append(root[:len(root)-1], 'b')
				return root
			}(),
			errExpected: true,
		},
		{
			desc: "correct size and a valid value",
			root: func() []byte {
				root := testproof.SmstRootWithSumAndCount(numRelays, numRelays)
				return root[:]
			}(),
			errExpected: false,
		},
	}

	// Iterate over each test case
	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			// Setup claim by copying the testproof.BaseClaim and updating the root
			claim := testproof.BaseClaim(service.Id, appAddr, supplierOperatorAddr, 0)
			claim.RootHash = smt.MerkleRoot(test.root[:])
			pendingResult := tlm.NewClaimSettlementResult(claim)

			settlementContext := tokenomicskeeper.NewSettlementContext(ctx, &keeper, keeper.Logger())

			// Ignoring the error from ClaimCacheWarmUp as it will short-circuit the test
			// and we want to test the error from ProcessTokenLogicModules.
			_ = settlementContext.ClaimCacheWarmUp(ctx, &claim)

			// Execute test function
			err := keeper.ProcessTokenLogicModules(ctx, settlementContext, pendingResult)

			// Assert the error
			if test.errExpected {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestProcessTokenLogicModules_InvalidClaim(t *testing.T) {
	keeper, ctx, appAddr, supplierOperatorAddr, service := testkeeper.TokenomicsKeeperWithActorAddrs(t)
	numRelays := uint64(42)

	// Define test cases
	tests := []struct {
		desc        string
		claim       prooftypes.Claim
		errExpected bool
		expectErr   error
	}{

		{
			desc: "Valid claim",
			claim: func() prooftypes.Claim {
				claim := testproof.BaseClaim(service.Id, appAddr, supplierOperatorAddr, numRelays)
				return claim
			}(),
			errExpected: false,
		},
		{
			desc: "claim with nil session header",
			claim: func() prooftypes.Claim {
				claim := testproof.BaseClaim(service.Id, appAddr, supplierOperatorAddr, numRelays)
				claim.SessionHeader = nil
				return claim
			}(),
			errExpected: true,
			expectErr:   tokenomicstypes.ErrTokenomicsSessionHeaderNil,
		},
		{
			desc: "claim with invalid session id",
			claim: func() prooftypes.Claim {
				claim := testproof.BaseClaim(service.Id, appAddr, supplierOperatorAddr, numRelays)
				claim.SessionHeader.SessionId = ""
				return claim
			}(),
			errExpected: true,
			expectErr:   tokenomicstypes.ErrTokenomicsSessionHeaderInvalid,
		},
		{
			desc: "claim with invalid application address",
			claim: func() prooftypes.Claim {
				claim := testproof.BaseClaim(service.Id, appAddr, supplierOperatorAddr, numRelays)
				claim.SessionHeader.ApplicationAddress = "invalid address"
				return claim
			}(),
			errExpected: true,
			expectErr:   tokenomicstypes.ErrTokenomicsSessionHeaderInvalid,
		},
		{
			desc: "claim with invalid supplier operator address",
			claim: func() prooftypes.Claim {
				claim := testproof.BaseClaim(service.Id, appAddr, supplierOperatorAddr, numRelays)
				claim.SupplierOperatorAddress = "invalid address"
				return claim
			}(),
			errExpected: true,
			expectErr:   tokenomicstypes.ErrTokenomicsSupplierNotFound,
		},
	}

	// Iterate over each test case
	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			// Execute test function
			err := func() (err error) {
				defer func() {
					if r := recover(); r != nil {
						err = fmt.Errorf("panic occurred: %v", r)
					}
				}()
				pendingResult := tlm.NewClaimSettlementResult(test.claim)

				settlementContext := tokenomicskeeper.NewSettlementContext(ctx, &keeper, keeper.Logger())

				// Ignoring the error from ClaimCacheWarmUp as it will short-circuit the test
				// and we want to test the error from ProcessTokenLogicModules.
				_ = settlementContext.ClaimCacheWarmUp(ctx, &test.claim)
				return keeper.ProcessTokenLogicModules(ctx, settlementContext, pendingResult)
			}()

			// Assert the error
			if test.errExpected {
				require.Error(t, err)
				require.ErrorIs(t, err, test.expectErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestProcessTokenLogicModules_AppStakeInsufficientToCoverGlobalInflationAmount(t *testing.T) {
	t.Skip("TODO_MAINNET_MIGRATION(@red-0ne): Test application stake that is insufficient to cover the global inflation amount, for reimbursment and the max claim should scale down proportionally")
}

func TestProcessTokenLogicModules_AppStakeTooLowRoundingToZero(t *testing.T) {
	t.Skip("TODO_MAINNET_MIGRATION(@red-0ne): Test application stake that is too low which results in stake/num_suppliers rounding down to zero")
}

func TestProcessTokenLogicModules_AppStakeDropsBelowMinStakeAfterSession(t *testing.T) {
	t.Skip("TODO_MAINNET_MIGRATION(@red-0ne): Test that application stake being auto-unbonding after the stake drops below the required minimum when settling session accounting")
}

// prepareTestClaim uses the given number of relays and compute unit per relay in the
// service provided to set up the test claim correctly.
func prepareTestClaim(
	numRelays uint64,
	service *sharedtypes.Service,
	app *apptypes.Application,
	supplier *sharedtypes.Supplier,
) prooftypes.Claim {
	numComputeUnits := numRelays * service.ComputeUnitsPerRelay
	return prooftypes.Claim{
		SupplierOperatorAddress: supplier.OperatorAddress,
		SessionHeader: &sessiontypes.SessionHeader{
			ApplicationAddress:      app.Address,
			ServiceId:               service.Id,
			SessionId:               "session_id",
			SessionStartBlockHeight: 1,
			SessionEndBlockHeight:   testsession.GetSessionEndHeightWithDefaultParams(1),
		},
		RootHash: testproof.SmstRootWithSumAndCount(numComputeUnits, numRelays),
	}
}

// prepareTestService creates a service with the given compute units per relay.
func prepareTestService(serviceComputeUnitsPerRelay uint64) *sharedtypes.Service {
	return &sharedtypes.Service{
		Id:                   "svc1",
		Name:                 "svcName1",
		ComputeUnitsPerRelay: serviceComputeUnitsPerRelay,
		OwnerAddress:         sample.AccAddress(),
	}
}

func getBalance(
	t *testing.T,
	ctx context.Context,
	bankKeeper tokenomicstypes.BankKeeper,
	accountAddr string,
) *cosmostypes.Coin {
	appBalanceRes, err := bankKeeper.Balance(ctx, &banktypes.QueryBalanceRequest{
		Address: accountAddr,
		Denom:   "upokt",
	})
	require.NoError(t, err)

	balance := appBalanceRes.GetBalance()
	require.NotNil(t, balance)

	return balance
}

// computeShare computes the share of the given amount based a percentage.
func computeShare(t *testing.T, amount *big.Rat, sharePercentage float64) cosmosmath.Int {
	amountRat, err := encoding.Float64ToRat(sharePercentage)
	require.NoError(t, err)

	mintRat := new(big.Rat).Mul(amount, amountRat)
	flooredShare := new(big.Int).Quo(mintRat.Num(), mintRat.Denom())

	return cosmosmath.NewIntFromBigInt(flooredShare)
}
