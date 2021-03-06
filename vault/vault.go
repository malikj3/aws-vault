package vault

import (
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/99designs/aws-vault/prompt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sts"
)

const defaultExpirationWindow = 5 * time.Minute

var UseSession = true
var UseSessionCache = true

func NewSession(creds *credentials.Credentials, region string) (*session.Session, error) {
	return session.NewSession(aws.NewConfig().WithRegion(region).WithCredentials(creds))
}

func FormatKeyForDisplay(k string) string {
	return fmt.Sprintf("****************%s", k[len(k)-4:])
}

// Mfa contains options for an MFA device
type Mfa struct {
	MfaToken        string
	MfaPromptMethod string
	MfaSerial       string
}

// GetMfaToken returns the MFA token
func (m *Mfa) GetMfaToken() (*string, error) {
	if m.MfaToken != "" {
		return aws.String(m.MfaToken), nil
	}

	if m.MfaPromptMethod != "" {
		promptFunc := prompt.Method(m.MfaPromptMethod)
		token, err := promptFunc(fmt.Sprintf("Enter token for %s: ", m.MfaSerial))
		return aws.String(token), err
	}

	return nil, errors.New("No prompt found")
}

// NewMasterCredentialsProvider creates a provider for the master credentials
func NewMasterCredentialsProvider(k *CredentialKeyring, credentialsName string) *KeyringProvider {
	return &KeyringProvider{k, credentialsName}
}

func NewMasterCredentials(k *CredentialKeyring, credentialsName string) *credentials.Credentials {
	return credentials.NewCredentials(NewMasterCredentialsProvider(k, credentialsName))
}

func NewSessionTokenProvider(creds *credentials.Credentials, k *CredentialKeyring, config *Config) (credentials.Provider, error) {
	sess, err := NewSession(creds, config.Region)
	if err != nil {
		return nil, err
	}

	sessionTokenProvider := &SessionTokenProvider{
		StsClient:    sts.New(sess),
		Duration:     config.GetSessionTokenDuration,
		ExpiryWindow: defaultExpirationWindow,
		Mfa: Mfa{
			MfaToken:        config.MfaToken,
			MfaPromptMethod: config.MfaPromptMethod,
			MfaSerial:       config.MfaSerial,
		},
	}

	if UseSessionCache {
		return &CachedSessionTokenProvider{
			Keyring:         k,
			CredentialsName: config.ProfileName,
			ExpiryWindow:    defaultExpirationWindow,
			Provider:        sessionTokenProvider,
		}, nil
	}

	return sessionTokenProvider, nil
}

// NewAssumeRoleProvider returns a provider that generates credentials using AssumeRole
func NewAssumeRoleProvider(creds *credentials.Credentials, config *Config, noMfa bool) (*AssumeRoleProvider, error) {
	sess, err := NewSession(creds, config.Region)
	if err != nil {
		return nil, err
	}

	mfa := config.MfaSerial
	if noMfa {
		mfa = ""
	}

	return &AssumeRoleProvider{
		StsClient:       sts.New(sess),
		RoleARN:         config.RoleARN,
		RoleSessionName: config.RoleSessionName,
		ExternalID:      config.ExternalID,
		Duration:        config.AssumeRoleDuration,
		ExpiryWindow:    defaultExpirationWindow,
		Mfa: Mfa{
			MfaSerial:       mfa,
			MfaToken:        config.MfaToken,
			MfaPromptMethod: config.MfaPromptMethod,
		},
	}, nil
}

// Provider creates a credential provider for the given config. To chain the MFA serial with a source credential, pass the MFA serial in chainMfaSerial
func NewTempCredentialsProvider(config *Config, keyring *CredentialKeyring) (credentials.Provider, error) {
	var sourceCredProvider credentials.Provider

	hasStoredCredentials, err := keyring.Has(config.ProfileName)
	if err != nil {
		return nil, err
	}

	if hasStoredCredentials {
		log.Printf("profile %s: using stored credentials %s", config.ProfileName, logSourceDetails(config))
		sourceCredProvider = NewMasterCredentialsProvider(keyring, config.ProfileName)
	} else if config.HasSourceProfile() {
		sourceCredProvider, err = NewTempCredentialsProvider(config.SourceProfile, keyring)
		if err != nil {
			return nil, err
		}
	} else {
		return nil, fmt.Errorf("profile %s: credentials missing", config.ProfileName)
	}

	mfaChained := config.MfaAlreadyUsedInSourceProfile()
	sourceCreds := credentials.NewCredentials(sourceCredProvider)

	if config.RoleARN == "" {
		if !UseSession {
			// log.Printf("profile %s: GetSessionToken disabled", config.ProfileName)
			config.MfaSerial = ""
			return sourceCredProvider, nil
		}

		if config.IsChained() {
			if !config.ChainedFromProfile.HasMfaSerial() {
				log.Printf("profile %s: not using GetSessionToken because profile '%s' has no MFA serial defined", config.ProfileName, config.ChainedFromProfile.ProfileName)
				return sourceCredProvider, nil
			}

			if config.ChainedFromProfile.MfaSerial != config.MfaSerial {
				log.Printf("profile %s: not using GetSessionToken because MFA serial doesn't match with profile '%s'", config.ProfileName, config.ChainedFromProfile.ProfileName)
				return sourceCredProvider, nil
			}

			config.GetSessionTokenDuration = config.ChainedGetSessionTokenDuration
		}

		log.Printf("profile %s: using GetSessionToken %s", config.ProfileName, mfaDetails(false, config))
		return NewSessionTokenProvider(sourceCreds, keyring, config)

	} else {
		log.Printf("profile %s: using AssumeRole %s", config.ProfileName, mfaDetails(mfaChained, config))
		return NewAssumeRoleProvider(sourceCreds, config, mfaChained)
	}
}

func logSourceDetails(config *Config) string {
	if config.SourceProfile != nil {
		return "(ignoring source_profile)"
	}
	return ""
}

func mfaDetails(mfaChained bool, config *Config) string {
	if mfaChained {
		return "(chained MFA)"
	}
	if config.HasMfaSerial() {
		return "(using MFA)"
	}
	return ""
}

// NewTempCredentials returns credentials for the given config
func NewTempCredentials(config *Config, k *CredentialKeyring) (*credentials.Credentials, error) {
	provider, err := NewTempCredentialsProvider(config, k)
	if err != nil {
		return nil, err
	}

	return credentials.NewCredentials(provider), nil
}

func NewFederationTokenCredentials(profileName string, k *CredentialKeyring, config *Config) (*credentials.Credentials, error) {
	credentialsName, err := MasterCredentialsFor(profileName, k, config)
	if err != nil {
		return nil, err
	}

	sess, err := NewSession(NewMasterCredentials(k, credentialsName), config.Region)
	if err != nil {
		return nil, err
	}

	currentUsername, err := GetUsernameFromSession(sess)
	if err != nil {
		return nil, err
	}

	log.Printf("Using GetFederationToken for credentials")
	return credentials.NewCredentials(&FederationTokenProvider{
		StsClient: sts.New(sess),
		Name:      currentUsername,
		Duration:  config.GetFederationTokenDuration,
	}), nil
}

func MasterCredentialsFor(profileName string, keyring *CredentialKeyring, config *Config) (string, error) {
	hasMasterCreds, err := keyring.Has(profileName)
	if err != nil {
		return "", err
	}

	if hasMasterCreds {
		return profileName, nil
	}

	return MasterCredentialsFor(config.SourceProfileName, keyring, config)
}
