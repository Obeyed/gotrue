package models

import (
	"database/sql"
	"time"

	"github.com/gobuffalo/pop/v5"
	"github.com/gofrs/uuid"
	"github.com/netlify/gotrue/crypto"
	"github.com/netlify/gotrue/storage"
	"github.com/pkg/errors"
)

// RefreshToken is the database model for refresh tokens.
type RefreshToken struct {
	InstanceID uuid.UUID `json:"-" db:"instance_id"`
	ID         int64     `db:"id"`

	Token  string             `db:"token"`
	Parent storage.NullString `db:"parent"`

	UserID uuid.UUID `db:"user_id"`
	User   *User     `belongs_to:"users"`

	SSOSession   *SSOSession `belongs_to:"sso_sessions"`
	SSOSessionID uuid.UUID   `db:"sso_session_id"`

	Revoked   bool      `db:"revoked"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

func (RefreshToken) TableName() string {
	tableName := "refresh_tokens"
	return tableName
}

// GrantAuthenticatedParams signals the parameters for gran
type GrantAuthenticatedConditions struct {
	SSOProviderID       uuid.UUID
	NotBefore           time.Time
	NotAfter            time.Time
	InitiatedByProvider bool
}

// GrantAuthenticatedUser creates a refresh token for the provided user.
func GrantAuthenticatedUser(tx *storage.Connection, user *User, cond *GrantAuthenticatedConditions) (*RefreshToken, error) {
	return createRefreshToken(tx, user, nil, cond)
}

// GrantRefreshTokenSwap swaps a refresh token for a new one, revoking the provided token.
func GrantRefreshTokenSwap(tx *storage.Connection, user *User, token *RefreshToken) (*RefreshToken, error) {
	var newToken *RefreshToken
	err := tx.Transaction(func(rtx *storage.Connection) error {
		var terr error
		if terr = NewAuditLogEntry(tx, user.InstanceID, user, TokenRevokedAction, "", nil); terr != nil {
			return errors.Wrap(terr, "error creating audit log entry")
		}

		token.Revoked = true
		if terr = tx.UpdateOnly(token, "revoked"); terr != nil {
			return terr
		}
		newToken, terr = createRefreshToken(rtx, user, token, nil)
		return terr
	})
	return newToken, err
}

// RevokeTokenFamily revokes all refresh tokens that descended from the provided token.
func RevokeTokenFamily(tx *storage.Connection, token *RefreshToken) error {
	tablename := (&pop.Model{Value: RefreshToken{}}).TableName()
	err := tx.RawQuery(`
	with recursive token_family as (
		select id, user_id, token, revoked, parent from `+tablename+` where parent = ?
		union
		select r.id, r.user_id, r.token, r.revoked, r.parent from `+tablename+` r inner join token_family t on t.token = r.parent
	)
	update `+tablename+` r set revoked = true from token_family where token_family.id = r.id;`, token.Token).Exec()
	if err != nil {
		return err
	}
	return nil
}

// GetValidChildToken returns the child token of the token provided if the child is not revoked.
func GetValidChildToken(tx *storage.Connection, token *RefreshToken) (*RefreshToken, error) {
	refreshToken := &RefreshToken{}
	err := tx.Q().Where("parent = ? and revoked = false", token.Token).First(refreshToken)
	if err != nil {
		if errors.Cause(err) == sql.ErrNoRows {
			return nil, RefreshTokenNotFoundError{}
		}
		return nil, err
	}
	return refreshToken, nil
}

// Logout deletes all refresh tokens for a user.
func Logout(tx *storage.Connection, instanceID uuid.UUID, id uuid.UUID) error {
	return tx.RawQuery("DELETE FROM "+(&pop.Model{Value: RefreshToken{}}).TableName()+" WHERE instance_id = ? AND user_id = ?", instanceID, id).Exec()
}

func createRefreshToken(tx *storage.Connection, user *User, oldToken *RefreshToken, cond *GrantAuthenticatedConditions) (*RefreshToken, error) {
	token := &RefreshToken{
		InstanceID: user.InstanceID,
		UserID:     user.ID,
		Token:      crypto.SecureToken(),
		Parent:     "",
	}

	if oldToken != nil {
		token.Parent = storage.NullString(oldToken.Token)
		token.SSOSessionID = oldToken.SSOSessionID
	} else if cond != nil {
		ssoSession := SSOSession{
			UserID:        user.ID,
			SSOProviderID: cond.SSOProviderID,

			IdPInitiated: cond.InitiatedByProvider,
			NotBefore:    cond.NotBefore,
			NotAfter:     cond.NotAfter,
		}

		if err := tx.Create(&ssoSession); err != nil {
			return nil, errors.Wrap(err, "error creating SSO session for refresh token")
		}

		token.SSOSession = &ssoSession
		token.SSOSessionID = ssoSession.ID
	}

	if err := tx.Create(token); err != nil {
		return nil, errors.Wrap(err, "error creating refresh token")
	}

	if err := tx.Eager().Q().Where("id = ?", token.ID).First(token); err != nil {
		return nil, errors.Wrap(err, "error loading refresh token after create")
	}

	if token.SSOSessionID != (uuid.UUID{}) {
		if err := tx.Eager().Q().Where("id = ?", token.SSOSessionID).First(token.SSOSession); err != nil {
			return nil, errors.Wrap(err, "error loading SSO session for refresh token after create")
		}

		ssoProvider := SSOProvider{}

		if err := tx.Eager().Q().Where("id = ?", token.SSOSession.SSOProviderID).First(&ssoProvider); err != nil {
			return nil, errors.Wrap(err, "error loading SSO provider for refresh token after create")
		}

		token.SSOSession.SSOProvider = &ssoProvider
	}

	if err := user.UpdateLastSignInAt(tx); err != nil {
		return nil, errors.Wrap(err, "error update user`s last_sign_in field")
	}

	return token, nil
}
