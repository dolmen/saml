package saml

import (
	"encoding/base64"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"io/ioutil"
	"net/http"
	"os"
	"sync/atomic"
)

// ServiceProvider represents a service provider.
type ServiceProvider struct {
	IdPMetadataURL string
	IdPMetadataXML []byte
	IdPMetadata    *Metadata

	KeyFile  string
	CertFile string

	PrivkeyPEM string
	PubkeyPEM  string

	MetadataURL string
	AcsURL      string

	DTDFile string

	AllowIdpInitiated bool

	SecurityOpts

	pemCert atomic.Value
}

// PrivkeyFile returns a physical path where the SP's key can be accessed.
func (sp *ServiceProvider) PrivkeyFile() (string, error) {
	if sp.KeyFile != "" {
		return sp.KeyFile, nil
	}
	if sp.PrivkeyPEM != "" {
		return writeFile([]byte(sp.PrivkeyPEM))
	}
	return "", errors.New("No private key given.")
}

// PubkeyFile returns a physical path where the SP's public certificate can be
// accessed.
func (sp *ServiceProvider) PubkeyFile() (string, error) {
	if sp.CertFile != "" {
		return validateKeyFile(sp.CertFile, nil)
	}
	if sp.PubkeyPEM != "" {
		return validateKeyFile(writeFile([]byte(sp.PubkeyPEM)))
	}
	return "", errors.New("No public key given.")
}

// GetIdPAuthResource returns the authentication URL for the SP.
func (sp *ServiceProvider) GetIdPAuthResource() (string, error) {
	meta, err := sp.GetIdPMetadata()
	if err != nil {
		return "", err
	}

	if meta.IDPSSODescriptor == nil {
		return "", errors.New("could not find IDPSSODescriptor")
	}

	for _, endpoint := range meta.IDPSSODescriptor.SingleSignOnService {
		if endpoint.Binding == "urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" {
			return endpoint.Location, nil
		}
		if endpoint.Binding == "urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST" {
			return endpoint.Location, nil
		}
	}

	return "", errors.New("could not find SingleSignOnService")
}

// GetIdPCertFile returns a physical path where the IdP certificate can be
// accessed.
func (sp *ServiceProvider) GetIdPCertFile() (string, error) {
	meta, err := sp.GetIdPMetadata()
	if err != nil {
		return "", err
	}

	cert := ""
	for _, keyDescriptor := range meta.IDPSSODescriptor.KeyDescriptor {
		if keyDescriptor.Use == "encryption" {
			cert = keyDescriptor.KeyInfo.Certificate
			break
		}
	}

	if cert == "" {
		for _, keyDescriptor := range meta.IDPSSODescriptor.KeyDescriptor {
			if keyDescriptor.KeyInfo.Certificate != "" {
				cert = keyDescriptor.KeyInfo.Certificate
				break
			}
		}
	}

	if cert == "" {
		return "", errors.New("Missing certificate data.")
	}

	certBytes, _ := base64.StdEncoding.DecodeString(cert)

	certBytes = pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certBytes,
	})

	return writeFile(certBytes)
}

// GetIdPMetadata returns the IdP metadata value.
func (sp *ServiceProvider) GetIdPMetadata() (*Metadata, error) {
	if sp.IdPMetadata != nil {
		m := *(sp.IdPMetadata)
		return &m, nil
	}

	if len(sp.IdPMetadataXML) == 0 {
		if sp.IdPMetadataURL == "" {
			return nil, errors.New("Missing metadata URL.")
		}

		res, err := http.Get(sp.IdPMetadataURL)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()

		buf, err := ioutil.ReadAll(res.Body)
		if err != nil {
			return nil, err
		}

		sp.IdPMetadataXML = buf
	}

	var metadata Metadata
	err := xml.Unmarshal(sp.IdPMetadataXML, &metadata)
	if err != nil {
		return nil, err
	}

	sp.IdPMetadata = &metadata
	return &metadata, nil
}

// Cert returns a *pem.Block value that corresponds to the SP's certificate.
func (sp *ServiceProvider) Cert() (*pem.Block, error) {
	if v := sp.pemCert.Load(); v != nil {
		return v.(*pem.Block), nil
	}

	certFile, err := sp.PubkeyFile()
	if err != nil {
		return nil, err
	}

	fp, err := os.Open(certFile)
	if err != nil {
		return nil, err
	}
	defer fp.Close()

	buf, err := ioutil.ReadAll(fp)
	if err != nil {
		return nil, err
	}

	cert, _ := pem.Decode(buf)
	if cert == nil {
		return nil, errors.New("Invalid certificate.")
	}

	sp.pemCert.Store(cert)

	return cert, nil
}

// Metadata returns a metadata value based on the SP's data.
func (sp *ServiceProvider) Metadata() (*Metadata, error) {
	cert, err := sp.Cert()
	if err != nil {
		return nil, err
	}
	certStr := base64.StdEncoding.EncodeToString(cert.Bytes)

	metadata := &Metadata{
		EntityID:   sp.MetadataURL,
		ValidUntil: Now().Add(defaultValidDuration),
		SPSSODescriptor: &SPSSODescriptor{
			AuthnRequestsSigned:        false,
			WantAssertionsSigned:       true,
			ProtocolSupportEnumeration: "urn:oasis:names:tc:SAML:2.0:protocol",
			KeyDescriptor: []KeyDescriptor{
				KeyDescriptor{
					Use: "signing",
					KeyInfo: KeyInfo{
						Certificate: certStr,
					},
				},
				KeyDescriptor{
					Use: "encryption",
					KeyInfo: KeyInfo{
						Certificate: certStr,
					},
					EncryptionMethods: []EncryptionMethod{
						EncryptionMethod{Algorithm: "http://www.w3.org/2001/04/xmlenc#aes128-cbc"},
						EncryptionMethod{Algorithm: "http://www.w3.org/2001/04/xmlenc#aes192-cbc"},
						EncryptionMethod{Algorithm: "http://www.w3.org/2001/04/xmlenc#aes256-cbc"},
						EncryptionMethod{Algorithm: "http://www.w3.org/2001/04/xmlenc#rsa-oaep-mgf1p"},
					},
				},
			},
			AssertionConsumerService: []IndexedEndpoint{{
				Binding:  HTTPPostBinding,
				Location: sp.AcsURL,
				Index:    1,
			}},
		},
	}

	return metadata, nil
}

// NewAuthnRequest creates a new AuthnRequest object for the given IdP URL.
func (sp *ServiceProvider) NewAuthnRequest(idpURL string) (*AuthnRequest, error) {
	req := AuthnRequest{
		AssertionConsumerServiceURL: sp.AcsURL,
		Destination:                 idpURL,
		ID:                          NewID(),
		IssueInstant:                Now(),
		Version:                     "2.0",
		Issuer: Issuer{
			Format: "urn:oasis:names:tc:SAML:2.0:nameid-format:entity",
			Value:  sp.MetadataURL,
		},
		NameIDPolicy: NameIDPolicy{
			AllowCreate: true,
			// TODO(ross): figure out exactly policy we need
			// urn:mace:shibboleth:1.0:nameIdentifier
			// urn:oasis:names:tc:SAML:2.0:nameid-format:transient
			Format: "urn:oasis:names:tc:SAML:2.0:nameid-format:transient",
		},
	}
	return &req, nil
}
