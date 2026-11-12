package arbitrage

import (
	"context"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// providerExecutor wraps the OpenAI-compatible executor under the arbitration
// Provider's own name.
//
// Why wrap instead of prefixing: KEEPER aggregates health per
// (model, provider) straight out of usage_events, where `provider` comes from
// the executor's Identifier(). If the runtime credential were published under a
// synthetic key like "openai-compatible-aliyun", every arbitrated request would
// land in usage under that key and KEEPER's health window would never observe
// the real `aliyun` Provider it prices — the feedback loop that lets KEEPER
// re-bid on measured latency/failure rate would stay open. Reporting the raw
// Provider name keeps price, health and routing all keyed the same way.
//
// Execution semantics are unchanged: the embedded executor still resolves
// base_url / api_key from Auth.Attributes and speaks the OpenAI-compatible
// protocol.
type providerExecutor struct {
	*executor.OpenAICompatExecutor
	provider string
}

// WrapProviderExecutor returns an executor for provider that delegates every
// operation to the OpenAI-compatible implementation but reports the raw
// Provider name as its Identifier.
func WrapProviderExecutor(provider string, cfg *config.Config) coreauth.ProviderExecutor {
	name := strings.ToLower(strings.TrimSpace(provider))
	return &providerExecutor{
		OpenAICompatExecutor: executor.NewOpenAICompatExecutor(name, cfg),
		provider:             name,
	}
}

// Identifier reports the arbitration Provider name so usage events, health
// aggregation and pricing all share one key.
func (e *providerExecutor) Identifier() string { return e.provider }

// Compile-time guards: the wrapper must remain a drop-in provider executor with
// the same optional capabilities the manager probes for.
var (
	_ coreauth.ProviderExecutor = (*providerExecutor)(nil)
	_ interface {
		HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error)
	} = (*providerExecutor)(nil)
	_ interface {
		Execute(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	} = (*providerExecutor)(nil)
)
