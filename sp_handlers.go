// Package sp provides tools for buildin an SP such as serving metadata,
// authenticating an assertion and building assertions for IdPs.
package saml

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"

	"github.com/goware/saml/xmlsec"
	"github.com/pkg/errors"
)

// SP-initiated login.
// AuthnRequestHandler creates SAML 2.0 AuthnRequest and sends
// it to the IdP via a HTTP 302 redirect. The data is passed in
// the ?SAMLRequest query parameter and the value is base64 encoded
// and deflate-compressed <AuthnRequest> XML element.
// Second query parameter, RelayState, represents a final redirect
// destination that will be invoked on successful login.
func (sp *ServiceProvider) AuthnRequestHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	destination, err := sp.GetIdPAuthResource()
	if err != nil {
		internalErr(w, errors.Errorf("GetIdPAuthResource: %v", err))
		return
	}

	authnRequest, err := sp.NewAuthnRequest(destination)
	if err != nil {
		internalErr(w, errors.Errorf("Failed to make auth request to %v: %v", destination, err))
		return
	}

	buf, err := xml.MarshalIndent(authnRequest, "", "\t")
	if err != nil {
		internalErr(w, errors.Errorf("Failed to marshal auth request %v", err))
		return
	}

	// RelayState is an opaque string that can be used to keep track of this
	// session on our side.
	var relayState string
	token := ctx.Value("saml.RelayState")
	if token != nil {
		relayState, _ = token.(string)
	}

	fbuf := bytes.NewBuffer(nil)
	fwri, err := flate.NewWriter(fbuf, flate.DefaultCompression)
	if err != nil {
		internalErr(w, errors.Errorf("Failed to build buffer %v", err))
		return
	}

	_, err = fwri.Write(buf)
	if err != nil {
		internalErr(w, errors.Errorf("Failed to write to buffer %v", err))
		return
	}
	fwri.Close()
	message := base64.StdEncoding.EncodeToString(fbuf.Bytes())

	redirectURL := destination + fmt.Sprintf(`?RelayState=%s&SAMLRequest=%s`, url.QueryEscape(relayState), url.QueryEscape(message))

	w.Header().Add("Location", redirectURL)
	w.WriteHeader(http.StatusFound)
}

// MetadataHandler serves SAML 2.0 Service Provider metadata XML file.
func (sp *ServiceProvider) MetadataHandler(w http.ResponseWriter, r *http.Request) {
	metadata, err := sp.Metadata()
	if err != nil {
		internalErr(w, errors.Wrapf(err, "could not build nor serve metadata XML"))
		return
	}
	out, err := xml.MarshalIndent(metadata, "", "\t")
	if err != nil {
		internalErr(w, errors.Wrapf(err, "could not format metadata"))
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf8")
	w.Write([]byte("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n"))
	w.Write(out)
}

func (sp *ServiceProvider) possibleResponseIDs() []string {
	responseIDs := []string{}
	if sp.AllowIdpInitiated {
		responseIDs = append(responseIDs, "")
	}
	return responseIDs
}

func (sp *ServiceProvider) verifySignature(plaintextMessage []byte) error {
	idpCertFile, err := sp.GetIdPCertFile()
	if err != nil {
		return err
	}

	err = xmlsec.Verify(plaintextMessage, idpCertFile, &xmlsec.ValidationOptions{
		DTDFile: sp.DTDFile,
	})
	if err == nil {
		// No error, this message is OK
		return nil
	}

	// We got an error...
	if !IsSecurityException(err, &sp.SecurityOpts) {
		// ...but it was not a security exception, so we ignore it and accept
		// the verification.
		return nil
	}

	return err
}

// AssertionMiddleware creates an HTTP handler that can be used to authenticate
// and validate an assertion. If the assertion is valid the flow it passed to
// the given grantFn function.
func (sp *ServiceProvider) AssertionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := Now()

		if err := parseFormAndKeepBody(r); err != nil {
			clientErr(w, r, errors.Wrap(err, "Unable to read POST data"))
		}

		samlResponse := r.Form.Get("SAMLResponse")

		// This RelayState (if any) needs to be been validated by the invoker.
		relayState := r.Form.Get("RelayState")

		// TODO: Remove this when we're stable enough.
		Logf("SAMLResponse -> %v", samlResponse)
		Logf("relayState -> %v", relayState)

		_ = relayState // Don't know what to do with this yet.

		samlResponseXML, err := base64.StdEncoding.DecodeString(samlResponse)
		if err != nil {
			err = errors.Wrapf(err, "could not decode base64 payload: %s", samlResponse)
			clientErr(w, r, errors.Wrap(err, "Malformed payload"))
			return
		}

		Logf("SAMLResponse (XML) -> %v", string(samlResponseXML))

		var res Response
		err = xml.Unmarshal(samlResponseXML, &res)
		if err != nil {
			err = errors.Wrapf(err, "could not unmarshal XML document: %s", string(samlResponseXML))
			clientErr(w, r, errors.Wrap(err, "Malformed XML"))
			return
		}

		_, err = sp.GetIdPMetadata()
		if err != nil {
			clientErr(w, r, errors.Wrap(err, "unable to retrieve IdP metadata"))
			return
		}

		// Validate message.

		if res.Destination != sp.AcsURL {
			// Note: OneLogin triggers this error when the Recipient field
			// is left blank (or when not set to the correct ACS endpoint)
			// in the OneLogin SAML configuration page. OneLogin returns
			// Destination="{recipient}" in the SAML reponse in this case.
			err := errors.Errorf("Wrong ACS destination, expecting %q, got %q", sp.AcsURL, res.Destination)
			clientErr(w, r, errors.Wrap(err, "Wrong ACS destination"))
			return
		}

		if sp.IdPMetadata.EntityID != "" {
			if res.Issuer == nil {
				clientErr(w, r, errors.New(`Missing "Issuer" node`))
				return
			}
			if res.Issuer.Value != sp.IdPMetadata.EntityID {
				err := errors.Errorf("Issuer %q does not match expected entity ID %q", res.Issuer.Value, sp.IdPMetadata.EntityID)
				clientErr(w, r, errors.Wrap(err, "Issuer does not match expected entity ID"))
				return
			}
		}

		if res.Status.StatusCode.Value != "urn:oasis:names:tc:SAML:2.0:status:Success" {
			err := errors.Errorf("Unexpected status code: %v", res.Status.StatusCode.Value)
			clientErr(w, r, errors.Wrap(err, "Unexpected status code"))
			return
		}

		expectedResponse := false
		responseIDs := sp.possibleResponseIDs()
		for i := range responseIDs {
			if responseIDs[i] == res.InResponseTo {
				expectedResponse = true
			}
		}
		if len(responseIDs) == 1 && responseIDs[0] == "" {
			expectedResponse = true
		}
		if !expectedResponse && len(responseIDs) > 0 {
			err := errors.Errorf("Expecting a proper InResponseTo value, got %#v", responseIDs)
			clientErr(w, r, err)
			return
		}

		// Try getting the IdP's cert file before using it.
		_, err = sp.GetIdPCertFile()
		if err != nil {
			internalErr(w, errors.Errorf("Failed to get private key: %v", err))
			return
		}

		// Validate signatures

		if res.Signature != nil {
			err := validateSignedNode(res.Signature, res.ID)
			if err != nil {
				clientErr(w, r, errors.Wrap(err, "failed to validate Response + Signature"))
				return
			}
		}

		if res.Assertion != nil && res.Assertion.Signature != nil {
			err := validateSignedNode(res.Assertion.Signature, res.Assertion.ID)
			if err != nil {
				clientErr(w, r, errors.Wrap(err, "failed to validate Assertion + Signature"))
				return
			}
		}

		// Validating message.
		signatureOK := false

		if res.Signature != nil || (res.Assertion != nil && res.Assertion.Signature != nil) {
			err := sp.verifySignature(samlResponseXML)
			if err != nil {
				clientErr(w, r, errors.Wrapf(err, "Unable to verify message signature"))
				return
			} else {
				signatureOK = true
			}
		}

		// Retrieve assertion
		var assertion *Assertion

		if res.EncryptedAssertion != nil {
			keyFile, err := sp.PrivkeyFile()
			if err != nil {
				internalErr(w, errors.Errorf("Failed to get private key: %v", err))
				return
			}

			plainTextAssertion, err := xmlsec.Decrypt(res.EncryptedAssertion.EncryptedData, keyFile)
			if err != nil {
				if IsSecurityException(err, &sp.SecurityOpts) {
					clientErr(w, r, errors.Wrap(err, "Unable to decrypt message"))
					return
				}
			}

			assertion = &Assertion{}
			if err := xml.Unmarshal(plainTextAssertion, assertion); err != nil {
				clientErr(w, r, errors.Wrap(err, "Unable to parse assertion"))
				return
			}

			if assertion.Signature != nil {
				err := validateSignedNode(assertion.Signature, assertion.ID)
				if err != nil {
					clientErr(w, r, errors.Wrap(err, "failed to validate Assertion + Signature"))
					return
				}

				err = sp.verifySignature(plainTextAssertion)
				if err != nil {
					clientErr(w, r, errors.Wrapf(err, "Unable to verify assertion signature"))
					return
				} else {
					signatureOK = true
				}
			}
		} else {
			assertion = res.Assertion
		}
		if assertion == nil {
			clientErr(w, r, errors.New("Missing assertion"))
			return
		}

		// Did we receive a signature?
		if !signatureOK {
			clientErr(w, r, errors.New("Unable to validate signature: node not found"))
			return
		}

		// Validate assertion.
		{
			var err error
			switch {
			case sp.IdPMetadata.EntityID == "":
				// Skip issuer validation
			case res.Issuer == nil:
				err = errors.New(`missing Assertion > Issuer`)
			case assertion.Issuer.Value != sp.IdPMetadata.EntityID:
				err = errors.Errorf("Assertion issuer %q does not match expected entity ID %q", assertion.Issuer.Value, sp.IdPMetadata.EntityID)
			}
			if err != nil {
				clientErr(w, r, errors.Wrap(err, "Assertion issuer does not match expected entity ID"))
			}
		}

		// Validate recipient
		{
			var err error
			switch {
			case assertion.Subject == nil:
				err = errors.New(`missing Assertion > Subject`)
			case assertion.Subject.SubjectConfirmation == nil:
				err = errors.New(`missing Assertion > Subject > SubjectConfirmation`)
			case assertion.Subject.SubjectConfirmation.SubjectConfirmationData.Recipient != sp.AcsURL:
				err = errors.Errorf("unexpected assertion recipient, expecting %q, got %q", sp.AcsURL, assertion.Subject.SubjectConfirmation.SubjectConfirmationData.Recipient)
			}
			if err != nil {
				clientErr(w, r, errors.Wrapf(err, "invalid assertion recipient"))
				return
			}
		}

		// Make sure we have Conditions
		if assertion.Conditions == nil {
			clientErr(w, r, errors.New(`missing Assertion > Conditions`))
			return
		}

		// The NotBefore and NotOnOrAfter attributes specify time limits on the
		// validity of the assertion within the context of its profile(s) of use.
		// They do not guarantee that the statements in the assertion will be
		// correct or accurate throughout the validity period. The NotBefore
		// attribute specifies the time instant at which the validity interval
		// begins. The NotOnOrAfter attribute specifies the time instant at which
		// the validity interval has ended. If the value for either NotBefore or
		// NotOnOrAfter is omitted, then it is considered unspecified.
		{
			validFrom := assertion.Conditions.NotBefore
			if !validFrom.IsZero() && validFrom.After(now.Add(ClockDriftTolerance)) {
				err := errors.Errorf("Assertion conditions are not valid yet, got %v, current time is %v", validFrom, now)
				clientErr(w, r, errors.Wrap(err, "Assertion conditions are not valid yet"))
				return
			}
		}

		{
			validUntil := assertion.Conditions.NotOnOrAfter
			if !validUntil.IsZero() && validUntil.Before(now.Add(-ClockDriftTolerance)) {
				err := errors.Errorf("Assertion conditions already expired, got %v current time is %v, extra time is %v", validUntil, now, now.Add(-ClockDriftTolerance))
				clientErr(w, r, errors.Wrap(err, "Assertion conditions already expired"))
				return
			}
		}

		// A time instant at which the subject can no longer be confirmed. The time
		// value is encoded in UTC, as described in Section 1.3.3.
		//
		// Note that the time period specified by the optional NotBefore and
		// NotOnOrAfter attributes, if present, SHOULD fall within the overall
		// assertion validity period as specified by the element's NotBefore and
		// NotOnOrAfter attributes. If both attributes are present, the value for
		// NotBefore MUST be less than (earlier than) the value for NotOnOrAfter.

		if validUntil := assertion.Subject.SubjectConfirmation.SubjectConfirmationData.NotOnOrAfter; validUntil.Before(now.Add(-ClockDriftTolerance)) {
			err := errors.Errorf("Assertion conditions already expired, got %v current time is %v", validUntil, now)
			clientErr(w, r, errors.Wrap(err, "Assertion conditions already expired"))
			return
		}

		// if assertion.Conditions != nil && assertion.Conditions.AudienceRestriction != nil {
		//   if assertion.Conditions.AudienceRestriction.Audience.Value != sp.MetadataURL {
		//     clientErr(w, fmt.Errorf("Audience restriction mismatch, got %q, expecting %q", assertion.Conditions.AudienceRestriction.Audience.Value, sp.MetadataURL), errors.New("Audience restriction mismatch"))
		//     return
		//   }
		// }

		expectedResponse = false
		for i := range responseIDs {
			if responseIDs[i] == assertion.Subject.SubjectConfirmation.SubjectConfirmationData.InResponseTo {
				expectedResponse = true
			}
		}
		if len(responseIDs) == 1 && responseIDs[0] == "" {
			expectedResponse = true
		}

		if !expectedResponse && len(responseIDs) > 0 {
			clientErr(w, r, errors.New("Unexpected assertion InResponseTo value"))
			return
		}

		ctx := context.WithValue(r.Context(), "saml.assertion", assertion)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func parseFormAndKeepBody(r *http.Request) error {
	var buf bytes.Buffer

	// Fill buf while reading r.Body
	r.Body = ioutil.NopCloser(io.TeeReader(r.Body, &buf))

	// ParseForm reads all data from r.Body and empties it because it's a buffer.
	if err := r.ParseForm(); err != nil {
		return err
	}

	// Restore body so it can be read again.
	r.Body = ioutil.NopCloser(&buf)
	return nil
}

func publicErrorMessage(err error) string {
	type causer interface {
		Cause() error
	}
	// is there any better way to retrieve the error _message_ without the cause?
	if _, ok := err.(causer); ok {
		msg := err.Error()
		parts := strings.SplitN(msg, ":", 2)
		if len(parts) > 0 {
			return parts[0]
		}
		return msg
	}
	return err.Error()
}

func clientErr(w http.ResponseWriter, r *http.Request, err error) {
	publicError := publicErrorMessage(err)

	report := InspectRequest(r)

	Fatal(fmt.Errorf("failed request: %v, details: %s", err, report.String()))

	w.Header().Set("Content-Type", "text/plain; charset=utf8")
	w.WriteHeader(http.StatusBadRequest)
	w.Write([]byte(publicError))
}

func internalErr(w http.ResponseWriter, err error) {
	serverErr(w, errors.Wrap(err, "an internal error ocurred, please try again"))
}

func serverErr(w http.ResponseWriter, err error) {
	Fatal(err)

	w.Header().Set("Content-Type", "text/plain; charset=utf8")
	w.WriteHeader(http.StatusInternalServerError)
	w.Write([]byte(publicErrorMessage(err)))
}

func validateSignedNode(signature *xmlsec.Signature, nodeID string) error {
	signatureURI := signature.Reference.URI
	if signatureURI == "" {
		return nil
	}
	if strings.HasPrefix(signatureURI, "#") {
		if nodeID == signatureURI[1:] {
			return nil
		}
		return fmt.Errorf("signed Reference.URI %q does not match ID %v", signatureURI, nodeID)
	}
	return fmt.Errorf("cannot lookup external URIs (%q)", signatureURI)
}
