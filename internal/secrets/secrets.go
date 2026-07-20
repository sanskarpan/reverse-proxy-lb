package secrets

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	vault "github.com/hashicorp/vault/api"
)

// reBraced matches ${VARNAME} placeholders (VARNAME may contain letters, digits, underscores).
var reBraced = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// reUnbraced matches $VARNAME placeholders not followed by { (word boundary aware).
var reUnbraced = regexp.MustCompile(`\$([A-Za-z_][A-Za-z0-9_]*)`)

// reVault matches ${vault:PATH#KEY} placeholders.
var reVault = regexp.MustCompile(`\$\{vault:([^#}]+)#([^}]+)\}`)

// Expand replaces all occurrences of ${VAR} and $VAR with the corresponding
// environment variable value. If the environment variable is not set, the
// placeholder is left as-is (not replaced with an empty string).
func Expand(s string) string {
	// Replace ${VAR} first.
	s = reBraced.ReplaceAllStringFunc(s, func(match string) string {
		// Extract the variable name from ${VARNAME}.
		name := match[2 : len(match)-1]
		val, ok := os.LookupEnv(name)
		if !ok {
			return match
		}
		return val
	})

	// Replace $VAR (not preceded by ${).
	s = reUnbraced.ReplaceAllStringFunc(s, func(match string) string {
		// Extract the variable name from $VARNAME.
		name := match[1:]
		val, ok := os.LookupEnv(name)
		if !ok {
			return match
		}
		return val
	})

	return s
}

// VaultClient wraps a *vault.Client and a KV v2 mount path.
type VaultClient struct {
	client    *vault.Client
	mountPath string
}

// NewVaultClient creates a new VaultClient connecting to addr with the given
// token, reading KV v2 secrets from mountPath.
func NewVaultClient(addr, token, mountPath string) (*VaultClient, error) {
	cfg := vault.DefaultConfig()
	if addr != "" {
		cfg.Address = addr
	}

	c, err := vault.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("secrets: failed to create vault client: %w", err)
	}

	if token != "" {
		c.SetToken(token)
	}

	return &VaultClient{
		client:    c,
		mountPath: mountPath,
	}, nil
}

// Get fetches KV v2 secret data from <mountPath>/data/<path> and returns the
// inner data map (the string key/value pairs stored by the operator).
func (v *VaultClient) Get(ctx context.Context, path string) (map[string]string, error) {
	secretPath := fmt.Sprintf("%s/data/%s", v.mountPath, path)
	secret, err := v.client.Logical().ReadWithContext(ctx, secretPath)
	if err != nil {
		return nil, fmt.Errorf("secrets: vault read %q: %w", secretPath, err)
	}
	if secret == nil || secret.Data == nil {
		return nil, fmt.Errorf("secrets: vault returned empty data for path %q", secretPath)
	}

	// KV v2 wraps the actual data under secret.Data["data"].
	rawData, ok := secret.Data["data"]
	if !ok {
		return nil, fmt.Errorf("secrets: vault response missing 'data' key for path %q", secretPath)
	}

	dataMap, ok := rawData.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("secrets: vault 'data' for path %q is not a map", secretPath)
	}

	result := make(map[string]string, len(dataMap))
	for k, val := range dataMap {
		switch sv := val.(type) {
		case string:
			result[k] = sv
		default:
			result[k] = fmt.Sprintf("%v", val)
		}
	}
	return result, nil
}

// ExpandAll expands a slice of field strings. For each field:
//   - If it contains ${vault:PATH#KEY}, the secret is fetched from Vault and
//     the placeholder is replaced with the secret value.
//   - Otherwise Expand() is called for environment-variable substitution.
//
// The returned slice preserves the input order. vc may be nil, in which case
// Vault placeholders are left unexpanded.
func ExpandAll(fields []string, vc *VaultClient, ctx context.Context) ([]string, error) {
	out := make([]string, len(fields))

	// Cache fetched Vault paths to avoid redundant API calls.
	cache := make(map[string]map[string]string)

	for i, f := range fields {
		expanded, err := expandField(f, vc, ctx, cache)
		if err != nil {
			return nil, err
		}
		out[i] = expanded
	}

	return out, nil
}

// expandField handles a single field value: replaces ${vault:PATH#KEY}
// placeholders using the VaultClient, then calls Expand() for env-var
// substitution.
func expandField(s string, vc *VaultClient, ctx context.Context, cache map[string]map[string]string) (string, error) {
	if vc == nil || !strings.Contains(s, "${vault:") {
		return Expand(s), nil
	}

	var lastErr error
	result := reVault.ReplaceAllStringFunc(s, func(match string) string {
		if lastErr != nil {
			return match
		}

		// Parse PATH and KEY from ${vault:PATH#KEY}.
		sub := reVault.FindStringSubmatch(match)
		if len(sub) != 3 {
			return match
		}
		path, key := sub[1], sub[2]

		data, ok := cache[path]
		if !ok {
			var err error
			data, err = vc.Get(ctx, path)
			if err != nil {
				lastErr = err
				return match
			}
			cache[path] = data
		}

		val, ok := data[key]
		if !ok {
			lastErr = fmt.Errorf("secrets: key %q not found in vault path %q", key, path)
			return match
		}
		return val
	})

	if lastErr != nil {
		return "", lastErr
	}

	// After Vault expansion, also expand any remaining env-var placeholders.
	return Expand(result), nil
}
