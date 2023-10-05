package criteria

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"net"
	"regexp"
	"strings"

	"github.com/open-policy-agent/opa/ast"

	"github.com/pomerium/pomerium/pkg/policy/generator"
	"github.com/pomerium/pomerium/pkg/policy/parser"
)

var clientCertificateBaseBody = ast.MustParseBody(`
	cert := crypto.x509.parse_certificates(trim_space(input.http.client_certificate.leaf))[0]
	fingerprint := crypto.sha256(base64.decode(cert.Raw))
	spki_hash := base64.encode(hex.decode(
		crypto.sha256(base64.decode(cert.RawSubjectPublicKeyInfo))))
	san_email_addresses := cert.EmailAddresses
	san_dns_names := cert.DNSNames
	san_ip_addresses := cert.IPAddresses
	san_uris := cert.URIs
`)

type clientCertificateCriterion struct {
	g *Generator
}

func (clientCertificateCriterion) DataType() generator.CriterionDataType {
	return CriterionDataTypeCertificateMatcher
}

func (clientCertificateCriterion) Name() string {
	return "client_certificate"
}

func (c clientCertificateCriterion) GenerateRule(
	_ string, data parser.Value,
) (*ast.Rule, []*ast.Rule, error) {
	body := append(ast.Body(nil), clientCertificateBaseBody...)

	obj, ok := data.(parser.Object)
	if !ok {
		return nil, nil, fmt.Errorf("expected object for certificate matcher, got: %T", data)
	}

	for k, v := range obj {
		var err error

		switch k {
		case "fingerprint":
			err = addCertFingerprintCondition(&body, v)
		case "spki_hash":
			err = addCertSPKIHashCondition(&body, v)
		case "email":
			err = addSanEmailCondition(&body, v)
		case "dns":
			err = addSanDNSCondition(&body, v)
		case "ip":
			err = addSanIPCondition(&body, v)
		case "uri":
			err = addSanURICondition(&body, v)
		default:
			err = fmt.Errorf("unsupported certificate matcher condition: %s", k)
		}

		if err != nil {
			return nil, nil, err
		}
	}

	rule := NewCriterionRule(c.g, c.Name(),
		ReasonClientCertificateOK, ReasonClientCertificateUnauthorized,
		body)

	return rule, nil, nil
}

func addCertFingerprintCondition(body *ast.Body, data parser.Value) error {
	var pa parser.Array
	switch v := data.(type) {
	case parser.Array:
		pa = v
	case parser.String:
		pa = parser.Array{data}
	default:
		return errors.New("certificate fingerprint condition expects a string or array of strings")
	}

	ra := ast.NewArray()
	for _, v := range pa {
		f, err := canonicalCertFingerprint(v)
		if err != nil {
			return err
		}
		ra = ra.Append(ast.NewTerm(f))
	}

	*body = append(*body,
		ast.Assign.Expr(ast.VarTerm("allowed_fingerprints"), ast.NewTerm(ra)),
		ast.Equal.Expr(ast.VarTerm("fingerprint"), ast.VarTerm("allowed_fingerprints[_]")))
	return nil
}

// The long certificate fingerprint format is 32 uppercase hex-encoded bytes
// separated by colons.
var longCertFingerprintRE = regexp.MustCompile("^[0-9A-F]{2}(:[0-9A-F]{2}){31}$")

// The short certificate fingerprint format is 32 lowercase hex-encoded bytes.
var shortCertFingerprintRE = regexp.MustCompile("^[0-9a-f]{64}$")

// canonicalCertFingeprint converts a single fingerprint value into the format
// that our Rego logic generates.
func canonicalCertFingerprint(data parser.Value) (ast.Value, error) {
	s, ok := data.(parser.String)
	if !ok {
		return nil, fmt.Errorf("certificate fingerprint must be a string (was %v)", data)
	}

	f := string(s)
	if f == "" {
		return nil, errors.New("certificate fingerprint must not be empty")
	} else if shortCertFingerprintRE.MatchString(f) {
		return ast.String(f), nil
	} else if longCertFingerprintRE.MatchString(f) {
		f = strings.ToLower(strings.ReplaceAll(f, ":", ""))
		return ast.String(f), nil
	}
	return nil, fmt.Errorf("unsupported certificate fingerprint format (%s)", f)
}

func addCertSPKIHashCondition(body *ast.Body, data parser.Value) error {
	var pa parser.Array
	switch v := data.(type) {
	case parser.Array:
		pa = v
	case parser.String:
		pa = parser.Array{data}
	default:
		return errors.New("certificate SPKI hash condition expects a string or array of strings")
	}

	ra := ast.NewArray()
	for _, v := range pa {
		s, ok := v.(parser.String)
		if !ok {
			return fmt.Errorf("certificate SPKI hash must be a string (was %v)", v)
		}

		h := string(s)
		if h == "" {
			return errors.New("certificate SPKI hash must not be empty")
		} else if b, err := base64.StdEncoding.DecodeString(h); err != nil || len(b) != 32 {
			return fmt.Errorf("certificate SPKI hash must be a base64-encoded SHA-256 hash "+
				"(was %s)", h)
		}

		ra = ra.Append(ast.NewTerm(ast.String(h)))
	}

	*body = append(*body,
		ast.Assign.Expr(ast.VarTerm("allowed_spki_hashes"), ast.NewTerm(ra)),
		ast.Equal.Expr(ast.VarTerm("spki_hash"), ast.VarTerm("allowed_spki_hashes[_]")))
	return nil
}

func addSanEmailCondition(body *ast.Body, data parser.Value) error {
	var pa parser.Array
	switch v := data.(type) {
	case parser.Array:
		pa = v
	case parser.String:
		pa = parser.Array{data}
	default:
		return errors.New("certificate SAN email condition expects a string or array of strings")
	}

	ra := ast.NewArray()
	for _, v := range pa {
		s, ok := v.(parser.String)
		if !ok {
			return fmt.Errorf("certificate SAN email must be a string (was %v)", v)
		}

		emailStr := string(s)
		if _, err := mail.ParseAddress(emailStr); err != nil {
			return fmt.Errorf("certificate SAN email must be a valid email address (was %s)", emailStr)
		}

		ra = ra.Append(ast.NewTerm(ast.String(emailStr)))
	}

	*body = append(*body,
		ast.Assign.Expr(ast.VarTerm("allowed_san_emails"), ast.NewTerm(ra)),
		ast.Equal.Expr(ast.VarTerm("allowed_san_emails[_]"), ast.VarTerm("san_email_addresses[_]")))
	return nil
}

func addSanDNSCondition(body *ast.Body, data parser.Value) error {
	var pa parser.Array
	switch v := data.(type) {
	case parser.Array:
		pa = v
	case parser.String:
		pa = parser.Array{data}
	default:
		return errors.New("certificate SAN dns condition expects a string or array of strings")
	}

	ra := ast.NewArray()
	for _, v := range pa {
		s, ok := v.(parser.String)
		if !ok {
			return fmt.Errorf("certificate SAN dns must be a string (was %v)", v)
		}

		dnsStr := string(s)
		// regex extracted from govalidator
		const DNSName string = `^([a-zA-Z0-9_]{1}[a-zA-Z0-9_-]{0,62}){1}(\.[a-zA-Z0-9_]{1}[a-zA-Z0-9_-]{0,62})*[\._]?$`
		rxDNSName := regexp.MustCompile(DNSName)

		if !rxDNSName.MatchString(dnsStr) {
			return fmt.Errorf("certificate SAN dns must be a valid DNS name (was %s)", dnsStr)
		}

		ra = ra.Append(ast.NewTerm(ast.String(dnsStr)))
	}

	*body = append(*body,
		ast.Assign.Expr(ast.VarTerm("allowed_san_dns_names"), ast.NewTerm(ra)),
		ast.Equal.Expr(ast.VarTerm("allowed_san_dns_names[_]"), ast.VarTerm("san_dns_names[_]")))
	return nil
}

func addSanIPCondition(body *ast.Body, data parser.Value) error {
	var pa parser.Array
	switch v := data.(type) {
	case parser.Array:
		pa = v
	case parser.String:
		pa = parser.Array{data}
	default:
		return errors.New("certificate SAN IP condition expects a string or array of strings")
	}

	ra := ast.NewArray()
	for _, v := range pa {
		s, ok := v.(parser.String)
		if !ok {
			return fmt.Errorf("certificate SAN IP must be a string (was %v)", v)
		}

		ipStr := string(s)
		if net.ParseIP(ipStr) == nil {
			return fmt.Errorf("certificate SAN IP must be a valid IP address (was %s)", ipStr)
		}

		ra = ra.Append(ast.NewTerm(ast.String(ipStr)))
	}

	*body = append(*body,
		ast.Assign.Expr(ast.VarTerm("allowed_san_ip_addresses"), ast.NewTerm(ra)),
		ast.Equal.Expr(ast.VarTerm("allowed_san_ip_addresses[_]"), ast.VarTerm("san_ip_addresses[_]")))
	return nil
}

func addSanURICondition(body *ast.Body, data parser.Value) error {
    var pa parser.Array
    switch v := data.(type) {
    case parser.Array:
        pa = v
    case parser.String:
        pa = parser.Array{data}
    default:
        return errors.New("certificate SAN URI condition expects a string or array of strings")
    }

    ra := ast.NewArray()
    for _, v := range pa {
        s, ok := v.(parser.String)
        if !ok {
            return fmt.Errorf("certificate URI must be a string (was %v)", v)
        }

        urlStr := string(s)
        if _, err := url.Parse(urlStr); err != nil {
            return fmt.Errorf("certificate SAN URI must be a valid URI (was %s)", urlStr)
        }

        ra = ra.Append(ast.NewTerm(ast.String(urlStr)))
    }

    // Assign the allowed_san_uris to the parsed URIs from the config
    *body = append(*body, ast.Assign.Expr(ast.VarTerm("allowed_san_uris"), ast.NewTerm(ra)))

    // Construct each URI in san_uris separately and check against allowed URIs
    uriCheckBody := ast.MustParseBody(`some san_uri_index; sprintf("%s://%s%s", [san_uris[san_uri_index].Scheme, san_uris[san_uri_index].Host, san_uris[san_uri_index].Path]) == allowed_san_uris[_]`)
    for _, expr := range uriCheckBody {
        *body = append(*body, expr)
    }

    return nil
}



// ClientCertificate returns a Criterion on a client certificate.
func ClientCertificate(generator *Generator) Criterion {
	return clientCertificateCriterion{g: generator}
}

func init() {
	Register(ClientCertificate)
}
