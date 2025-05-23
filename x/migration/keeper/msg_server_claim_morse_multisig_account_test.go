package keeper_test

import (
	"strconv"
	"testing"

	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	govtypes "github.com/cosmos/cosmos-sdk/x/gov/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/pokt-network/poktroll/testutil/events"
	keepertest "github.com/pokt-network/poktroll/testutil/keeper"
	"github.com/pokt-network/poktroll/testutil/sample"
	"github.com/pokt-network/poktroll/testutil/testmigration"
	"github.com/pokt-network/poktroll/x/migration/keeper"
	migrationtypes "github.com/pokt-network/poktroll/x/migration/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"

	crypto "github.com/cometbft/cometbft/crypto"
)

// Prevent strconv unused error
var _ = strconv.IntSize

func TestMsgServer_ClaimMorseMultiSigAccount_Success(t *testing.T) {
	k, ctx := keepertest.MigrationKeeper(t)
	srv := keeper.NewMsgServerImpl(k)

	// Generate and import Morse claimable accounts.
	numAccounts := 6
	_, accountState, err := testmigration.NewMorseStateExportAndAccountState(numAccounts, testmigration.AllUnstakedMorseMultiSigAccountActorType)
	require.NoError(t, err)

	accountStateHash, err := accountState.GetHash()
	require.NoError(t, err)

	_, err = srv.ImportMorseClaimableAccounts(ctx, &migrationtypes.MsgImportMorseClaimableAccounts{
		Authority:             authtypes.NewModuleAddress(govtypes.ModuleName).String(),
		MorseAccountState:     *accountState,
		MorseAccountStateHash: accountStateHash,
	})
	require.NoError(t, err)

	// Claim each MorseClaimableAccount (all of which SHOULD NOT be staked as onchain actor).
	for morseAccountIdx, morseAccount := range accountState.Accounts {
		// Generate the corresponding morse private key using the account slice index as a seed.
		morsePrivKeys := testmigration.GenMorsePrivateKeysForMultiSig(uint64(morseAccountIdx))
		pKeys := make([]crypto.PrivKey, len(morsePrivKeys))
		for i, priv := range morsePrivKeys {
			pKeys[i] = priv
		}

		// Claim the MorseClaimableAccount.
		msgClaim, err := migrationtypes.NewMsgClaimMorseMultiSigAccount(
			sample.AccAddress(),
			pKeys,
			sample.AccAddress(),
		)
		require.NoError(t, err)

		msgClaimRes, err := srv.ClaimMorseMultiSigAccount(ctx, msgClaim)
		require.NoError(t, err)

		// Construct and assert the expected response.
		sharedParams := sharedtypes.DefaultParams()
		expectedSessionEndHeight := sharedtypes.GetSessionEndHeight(&sharedParams, ctx.BlockHeight())
		expectedClaimedBalance := morseAccount.GetUnstakedBalance().
			Add(morseAccount.GetSupplierStake()).
			Add(morseAccount.GetApplicationStake())
		expectedRes := &migrationtypes.MsgClaimMorseAccountResponse{
			MorseSrcAddress:  msgClaim.GetMorseSrcAddress(),
			ClaimedBalance:   expectedClaimedBalance,
			SessionEndHeight: expectedSessionEndHeight,
		}
		require.Equal(t, expectedRes, msgClaimRes)

		// Assert that the persisted MorseClaimableAccount is updated.
		expectedMorseAccount := morseAccount
		expectedMorseAccount.ShannonDestAddress = msgClaim.GetShannonDestAddress()
		expectedMorseAccount.ClaimedAtHeight = ctx.BlockHeight()
		foundMorseAccount, found := k.GetMorseClaimableAccount(ctx, msgClaim.GetMorseSrcAddress())
		require.True(t, found)
		require.Equal(t, *expectedMorseAccount, foundMorseAccount)

		// Assert that an event is emitted for each claim.
		expectedEvent := &migrationtypes.EventMorseAccountClaimed{
			ShannonDestAddress: msgClaim.ShannonDestAddress,
			MorseSrcAddress:    msgClaim.GetMorseSrcAddress(),
			ClaimedBalance:     expectedClaimedBalance,
			SessionEndHeight:   expectedSessionEndHeight,
		}
		claimEvents := events.FilterEvents[*migrationtypes.EventMorseAccountClaimed](t, ctx.EventManager().Events())
		require.Equal(t, 1, len(claimEvents))
		require.Equal(t, expectedEvent, claimEvents[0])

		// Reset the event manager to isolate events between claims.
		ctx = ctx.WithEventManager(sdk.NewEventManager())
	}
}

func TestMsgServer_ClaimMorseMultiSigAccount_Error(t *testing.T) {
	k, ctx := keepertest.MigrationKeeper(t)
	srv := keeper.NewMsgServerImpl(k)

	// Generate and import a set of Morse claimable accounts:
	// - One unstaked
	// - One unstaked multisig
	// - One staked as an application
	// - One staked as a supplier
	numAccounts := 4
	_, accountState, err := testmigration.NewMorseStateExportAndAccountState(numAccounts, testmigration.RoundRobinAllMorseAccountActorTypes)
	require.NoError(t, err)

	accountStateHash, err := accountState.GetHash()
	require.NoError(t, err)

	_, err = srv.ImportMorseClaimableAccounts(ctx, &migrationtypes.MsgImportMorseClaimableAccounts{
		Authority:             authtypes.NewModuleAddress(govtypes.ModuleName).String(),
		MorseAccountState:     *accountState,
		MorseAccountStateHash: accountStateHash,
	})
	require.NoError(t, err)

	// Generate the corresponding morse private key using the account slice index as a seed.
	morsePrivKeys := testmigration.GenMorsePrivateKeysForMultiSig(1)

	pKeys := make([]crypto.PrivKey, len(morsePrivKeys))
	for i, priv := range morsePrivKeys {
		pKeys[i] = priv
	}

	// Claim the MorseClaimableAccount with a random Shannon address.
	msgClaim, err := migrationtypes.NewMsgClaimMorseMultiSigAccount(
		sample.AccAddress(),
		pKeys,
		sample.AccAddress(),
	)
	require.NoError(t, err)

	t.Run("invalid claim msg", func(t *testing.T) {
		// Copy the message and set the morse signature to nil.
		invalidMsgClaim := *msgClaim
		invalidMsgClaim.MorseSignature = nil

		expectedErr := status.Error(
			codes.InvalidArgument,
			migrationtypes.ErrMorseSignature.Wrapf(
				"signature verification failed",
			).Error(),
		)

		_, err = srv.ClaimMorseMultiSigAccount(ctx, &invalidMsgClaim)
		require.EqualError(t, err, expectedErr.Error())
	})

	t.Run("account not found", func(t *testing.T) {
		wrongMorsePrivKey := testmigration.GenMorsePrivateKey(99)

		pKeys := make([]crypto.PrivKey, 1)
		pKeys[0] = wrongMorsePrivKey

		// Generate a Morse account claim message for the same Shannon
		// address with a nonexistent Morse source account.
		invalidMsgClaim, claimErr := migrationtypes.NewMsgClaimMorseMultiSigAccount(
			msgClaim.GetShannonDestAddress(),
			pKeys,
			sample.AccAddress(),
		)
		require.NoError(t, claimErr)

		expectedErr := status.Error(
			codes.NotFound,
			migrationtypes.ErrMorseAccountClaim.Wrapf(
				"no morse claimable account exists with address %q",
				invalidMsgClaim.GetMorseSrcAddress(),
			).Error(),
		)

		_, err = srv.ClaimMorseMultiSigAccount(ctx, invalidMsgClaim)
		require.EqualError(t, err, expectedErr.Error())
	})

	t.Run("account already claimed (non-zero claimed_at_height)", func(t *testing.T) {
		// Set the claimed at height BUT NOT the Shannon destination address.
		morseClaimableAccount := *accountState.Accounts[1]
		morseClaimableAccount.ClaimedAtHeight = 10
		k.SetMorseClaimableAccount(ctx, morseClaimableAccount)

		expectedErr := status.Error(
			codes.FailedPrecondition,
			migrationtypes.ErrMorseAccountClaim.Wrapf(
				"morse address %q has already been claimed at height %d by shannon address %q",
				accountState.Accounts[1].GetMorseSrcAddress(),
				10,
				accountState.Accounts[1].GetShannonDestAddress(),
			).Error(),
		)

		_, err = srv.ClaimMorseMultiSigAccount(ctx, msgClaim)
		require.EqualError(t, err, expectedErr.Error())
	})

	t.Run("account already claimed (non-empty shannon_dest_address)", func(t *testing.T) {
		// Set the Shannon destination address BUT NOT the claimed at height.
		morseClaimableAccount := *accountState.Accounts[1]
		morseClaimableAccount.ClaimedAtHeight = 0
		morseClaimableAccount.ShannonDestAddress = sample.AccAddress()
		k.SetMorseClaimableAccount(ctx, morseClaimableAccount)

		expectedErr := status.Error(
			codes.FailedPrecondition,
			migrationtypes.ErrMorseAccountClaim.Wrapf(
				"morse address %q has already been claimed at height %d by shannon address %q",
				accountState.Accounts[1].GetMorseSrcAddress(),
				0,
				morseClaimableAccount.ShannonDestAddress,
			).Error(),
		)

		_, err = srv.ClaimMorseMultiSigAccount(ctx, msgClaim)
		require.EqualError(t, err, expectedErr.Error())
	})

}
