package keeper

import (
	"context"

	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/pokt-network/poktroll/app/volatile"
	apptypes "github.com/pokt-network/poktroll/x/application/types"
	migrationtypes "github.com/pokt-network/poktroll/x/migration/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// ClaimMorseApplication performs the following steps, given msg is valid and a
// MorseClaimableAccount exists for the given morse_src_address:
//   - Mint and transfer all tokens (unstaked balance plus application stake) of the
//     MorseClaimableAccount to the shannonDestAddress.
//   - Mark the MorseClaimableAccount as claimed (i.e. adding the shannon_dest_address
//     and claimed_at_height).
//   - Stake an application for the amount specified in the MorseClaimableAccount,
//     and the service specified in the msg.
func (k msgServer) ClaimMorseApplication(ctx context.Context, msg *migrationtypes.MsgClaimMorseApplication) (*migrationtypes.MsgClaimMorseApplicationResponse, error) {
	sdkCtx := cosmostypes.UnwrapSDKContext(ctx)
	logger := k.Logger().With("method", "ClaimMorseApplication")
	waiveMorseClaimGasFees := k.GetParams(sdkCtx).WaiveMorseClaimGasFees

	// Ensure that gas fees are NOT waived if one of the following is true:
	// - The claim is invalid
	// - Morse account has already been claimed
	// Claiming gas fees in the cases above ensures that we prevent spamming.
	//
	// Rationale:
	// 1. Morse claim txs MAY be signed by Shannon accounts which have 0upokt balances.
	//    For this reason, gas fees are waived (in the ante handler) for txs which
	//    contain ONLY (one or more) Morse claim messages.
	// 2. This exposes a potential resource exhaustion vector (or at least extends the
	//    attack surface area) where an attacker would be able to take advantage of
	//    the fact that tx signature verification gas costs MAY be avoided under
	//    certain conditions.
	// 3. ALL Morse account claim message handlers therefore SHOULD ensure that
	//    tx signature verification gas costs ARE applied if the claim is EITHER
	//    invalid OR if the given Morse account has already been claimed. The latter
	//    is necessary to mitigate a replay attack vector.
	var (
		morseClaimableAccount              migrationtypes.MorseClaimableAccount
		isFound, isValid, isAlreadyClaimed bool
	)
	defer func() {
		if waiveMorseClaimGasFees && (!isFound || !isValid || isAlreadyClaimed) {
			// Attempt to charge the waived gas fee for invalid claims.
			sdkCtx.GasMeter()
			// DEV_NOTE: Assuming that the tx containing this message was signed
			// by a non-multisig externally owned account (EOA); i.e. secp256k1,
			// conventionally. If this assumption is violated, the "wrong" gas
			// cost will be charged for the given key type.
			gas := k.accountKeeper.GetParams(ctx).SigVerifyCostSecp256k1
			sdkCtx.GasMeter().ConsumeGas(gas, "ante verify: secp256k1")
		}
	}()

	if err := msg.ValidateBasic(); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	// DEV_NOTE: It is safe to use MustAccAddressFromBech32 here because the
	// shannonDestAddress is validated in MsgClaimMorseApplication#ValidateBasic().
	shannonAccAddr := cosmostypes.MustAccAddressFromBech32(msg.ShannonDestAddress)

	// Ensure that a MorseClaimableAccount exists for the given morseSrcAddress.
	morseClaimableAccount, isFound = k.GetMorseClaimableAccount(
		sdkCtx,
		msg.GetMorseSrcAddress(),
	)
	if !isFound {
		return nil, status.Error(
			codes.NotFound,
			migrationtypes.ErrMorseApplicationClaim.Wrapf(
				"no morse claimable account exists with address %q",
				msg.GetMorseSrcAddress(),
			).Error(),
		)
	}

	// Ensure that the given MorseClaimableAccount has not already been claimed.
	if morseClaimableAccount.IsClaimed() {
		isAlreadyClaimed = true
		return nil, status.Error(
			codes.FailedPrecondition,
			migrationtypes.ErrMorseApplicationClaim.Wrapf(
				"morse address %q has already been claimed at height %d by shannon address %q",
				msg.GetMorseSrcAddress(),
				morseClaimableAccount.ClaimedAtHeight,
				morseClaimableAccount.ShannonDestAddress,
			).Error(),
		)
	}

	// ONLY allow claiming as an application actor account if the MorseClaimableAccount
	// WAS staked as an application AND NOT as a supplier. A claim of staked POKT
	// from Morse to Shannon SHOULD NOT allow applications or suppliers to bypass
	// the onchain unbonding period.
	if !morseClaimableAccount.SupplierStake.IsZero() {
		return nil, status.Error(
			codes.FailedPrecondition,
			migrationtypes.ErrMorseAccountClaim.Wrapf(
				"Morse account %q is staked as an supplier, please use `pocketd tx migration claim-supplier` instead",
				morseClaimableAccount.GetMorseSrcAddress(),
			).Error(),
		)
	}

	if !morseClaimableAccount.ApplicationStake.IsPositive() {
		return nil, status.Error(
			codes.FailedPrecondition,
			migrationtypes.ErrMorseAccountClaim.Wrapf(
				"Morse account %q is not staked as an application or supplier, please use `pocketd tx migration claim-account` instead",
				morseClaimableAccount.GetMorseSrcAddress(),
			).Error(),
		)
	}

	// Mint the totalTokens to the shannonDestAddress account balance.
	// The application stake is subsequently escrowed from the shannonDestAddress account balance.
	if err := k.MintClaimedMorseTokens(ctx, shannonAccAddr, morseClaimableAccount.TotalTokens()); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	// Set ShannonDestAddress & ClaimedAtHeight (claim).
	morseClaimableAccount.ShannonDestAddress = shannonAccAddr.String()
	morseClaimableAccount.ClaimedAtHeight = sdkCtx.BlockHeight()

	// Update the MorseClaimableAccount.
	k.SetMorseClaimableAccount(
		sdkCtx,
		morseClaimableAccount,
	)

	// Query for any existing application stake prior to staking.
	preClaimAppStake := cosmostypes.NewInt64Coin(volatile.DenomuPOKT, 0)
	foundApp, isFound := k.appKeeper.GetApplication(ctx, shannonAccAddr.String())
	if isFound {
		preClaimAppStake = *foundApp.Stake
	}

	// Stake (or update) the application.
	msgStakeApp := apptypes.NewMsgStakeApplication(
		shannonAccAddr.String(),
		preClaimAppStake.Add(morseClaimableAccount.GetApplicationStake()),
		[]*sharedtypes.ApplicationServiceConfig{msg.ServiceConfig},
	)

	app, err := k.appKeeper.StakeApplication(ctx, logger, msgStakeApp)
	if err != nil {
		// DEV_NOTE: If the error is non-nil, StakeApplication SHOULD ALWAYS return a gRPC status error.
		return nil, err
	}

	claimedAppStake := morseClaimableAccount.GetApplicationStake()
	sharedParams := k.sharedKeeper.GetParams(ctx)
	sessionEndHeight := sharedtypes.GetSessionEndHeight(&sharedParams, sdkCtx.BlockHeight())
	claimedUnstakedBalance := morseClaimableAccount.GetUnstakedBalance()

	// Emit an event which signals that the morse account has been claimed.
	event := migrationtypes.EventMorseApplicationClaimed{
		MorseSrcAddress:         msg.GetMorseSrcAddress(),
		ClaimedBalance:          claimedUnstakedBalance,
		ClaimedApplicationStake: claimedAppStake,
		SessionEndHeight:        sessionEndHeight,
		Application:             app,
	}
	if err = sdkCtx.EventManager().EmitTypedEvent(&event); err != nil {
		return nil, status.Error(
			codes.Internal,
			migrationtypes.ErrMorseApplicationClaim.Wrapf(
				"failed to emit event type %T: %v",
				&event,
				err,
			).Error(),
		)
	}

	// Return the response.
	return &migrationtypes.MsgClaimMorseApplicationResponse{
		MorseSrcAddress:         msg.GetMorseSrcAddress(),
		ClaimedBalance:          claimedUnstakedBalance,
		ClaimedApplicationStake: claimedAppStake,
		SessionEndHeight:        sessionEndHeight,
		Application:             app,
	}, nil
}
