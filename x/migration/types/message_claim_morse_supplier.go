package types

import (
	cometcrypto "github.com/cometbft/cometbft/crypto/ed25519"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	"github.com/cosmos/gogoproto/proto"

	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

var (
	_ sdk.Msg           = (*MsgClaimMorseSupplier)(nil)
	_ MorseClaimMessage = (*MsgClaimMorseSupplier)(nil)
)

// NewMsgClaimMorseSupplier creates a new MsgClaimMorseSupplier.
// If morsePrivateKey is provided (i.e. not nil), it is used to sign the message.
func NewMsgClaimMorseSupplier(
	shannonOwnerAddress string,
	shannonOperatorAddress string,
	morsePrivateKey cometcrypto.PrivKey,
	services []*sharedtypes.SupplierServiceConfig,
	shannonSigningAddr string,
) (*MsgClaimMorseSupplier, error) {
	msg := &MsgClaimMorseSupplier{
		ShannonOwnerAddress:    shannonOwnerAddress,
		ShannonOperatorAddress: shannonOperatorAddress,
		Services:               services,
		ShannonSigningAddress:  shannonSigningAddr,
	}

	if morsePrivateKey != nil {
		msg.MorsePublicKey = morsePrivateKey.PubKey().Bytes()

		if err := msg.SignMorseSignature(morsePrivateKey); err != nil {
			return nil, err
		}
	}

	return msg, nil
}

// ValidateBasic ensures that:
// - The shannon owner address is valid (i.e. it is a valid bech32 address).
// - The shannon operator address is valid (i.e. it is a valid bech32 address).
// - The supplier service configs are valid.
// - The morsePublicKey is valid.
// - The morseSrcAddress matches the public key.
// - The morseSignature is valid.
func (msg *MsgClaimMorseSupplier) ValidateBasic() error {
	if _, err := sdk.AccAddressFromBech32(msg.GetShannonOwnerAddress()); err != nil {
		return sdkerrors.ErrInvalidAddress.Wrapf(
			"invalid shannon owner address address (%s): %s",
			msg.GetShannonOwnerAddress(), err,
		)
	}

	if _, err := sdk.AccAddressFromBech32(msg.ShannonOperatorAddress); err != nil {
		return sdkerrors.ErrInvalidAddress.Wrapf(
			"invalid shannon operator address address (%s): %s",
			msg.GetShannonOperatorAddress(), err,
		)
	}

	if err := sharedtypes.ValidateSupplierServiceConfigs(msg.Services); err != nil {
		return ErrMorseSupplierClaim.Wrapf("invalid service configs: %s", err)
	}

	// Validate the Morse signature.
	if err := msg.ValidateMorseSignature(); err != nil {
		return err
	}

	return nil
}

// SignMorseSignature signs the given MsgClaimMorseApplication with the given Morse private key.
func (msg *MsgClaimMorseSupplier) SignMorseSignature(morsePrivKey cometcrypto.PrivKey) (err error) {
	signingMsgBz, err := msg.getSigningBytes()
	if err != nil {
		return ErrMorseSignature.Wrapf("unable to get signing bytes: %s", err)
	}

	msg.MorseSignature, err = morsePrivKey.Sign(signingMsgBz)
	if err != nil {
		return ErrMorseSignature.Wrapf("unable to sign message: %s", err)
	}

	return nil
}

// ValidateMorseSignature validates the signature of the given MsgClaimMorseSupplier
// matches the given Morse public key.
func (msg *MsgClaimMorseSupplier) ValidateMorseSignature() error {
	if len(msg.GetMorseSignature()) != MorseSignatureLengthBytes {
		return ErrMorseSignature.Wrapf(
			"invalid morse signature length; expected %d, got %d",
			MorseSignatureLengthBytes, len(msg.GetMorseSignature()),
		)
	}
	signingBz, err := msg.getSigningBytes()
	if err != nil {
		return err
	}

	if !msg.GetMorsePublicKey().VerifySignature(signingBz, msg.GetMorseSignature()) {
		return ErrMorseSignature.Wrapf(
			"morseSignature (%x) is invalid for Morse address (%s)",
			msg.GetMorseSignature(),
			msg.GetMorseSrcAddress(),
		)
	}
	return nil
}

// getSigningBytes returns the canonical byte representation of the MsgClaimMorseSupplier
// which is used for signing and/or signature validation.
func (msg *MsgClaimMorseSupplier) getSigningBytes() ([]byte, error) {
	// Copy msg and clear the morse signature field (ONLY on the copy) to prevent
	// it from being included in the signature validation.
	signingMsg := *msg
	signingMsg.MorseSignature = nil

	return proto.Marshal(&signingMsg)
}

// GetMorseSrcAddress returns the morse source address associated with
// the Morse public key of the given message.
func (msg *MsgClaimMorseSupplier) GetMorseSrcAddress() string {
	return msg.GetMorsePublicKey().Address().String()
}

// GetMorsePublicKeyBz returns the Amino-encoded public key of the given message.
func (msg *MsgClaimMorseSupplier) GetMorsePublicKeyBz() []byte {
	return msg.GetMorsePublicKey()
}

// GetShannonDestAddress returns the shannon destination address associated with
// the shannon public key of the given message.
func (msg *MsgClaimMorseSupplier) GetShannonDestAddress() string {
	return msg.GetShannonOperatorAddress()
}
