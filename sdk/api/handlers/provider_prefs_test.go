package handlers_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/routeboundary"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
)

// AC3 (security boundary priority): a client provider preference that names a
// provider outside the caller's KEEPER allow-list must be rejected with a
// route_boundary_violation, and no out-of-boundary provider's executor may be
// invoked. The boundary gate is the source of truth; the preference transform
// can only narrow the already-boundary-pool, never widen it.
func TestProviderPrefsOutOfBoundaryRejected(t *testing.T) {
	const model = "pp-model-inbound"
	const key = "sk-pp-inbound"
	// openai has a managed credential but is NOT in the allow-list.
	handler, executors, recorder, ginCtx := newRBGateHandler(t, model, []string{"openai", "anthropic"}, key, routeboundary.Boundary{
		APIKey:           key,
		Enabled:          true,
		AllowedProviders: []string{"anthropic"},
	})
	_ = recorder

	ctx := rbGateContext(ginCtx)
	body := `{"model":"` + model + `","provider":{"only":["openai"]}}`
	_, _, errMsg := handler.ExecuteWithAuthManager(ctx, "openai", model, []byte(body), "")
	if errMsg == nil {
		t.Fatalf("expected a route_boundary_violation, got success")
	}
	if errMsg.StatusCode != http.StatusForbidden && errMsg.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 403/503, got %d", errMsg.StatusCode)
	}
	if !strings_Contains(string(errMsg.Body), routeboundary.ErrorType) {
		t.Fatalf("violation body missing %s: %s", routeboundary.ErrorType, errMsg.Body)
	}
	// No out-of-boundary provider may have executed.
	if got := executors["openai"].CallCount(); got != 0 {
		t.Fatalf("openai executor invoked %d times; must stay 0 outside the boundary", got)
	}
}

// Zero-regression (AC5): a request carrying no provider object leaves the pool
// and the fallback chain untouched; a request whose provider object names only
// in-boundary providers succeeds exactly as before.
func TestProviderPrefsInBoundarySucceeds(t *testing.T) {
	const model = "pp-model-inbound"
	const key = "sk-pp-inbound"
	handler, executors, recorder, ginCtx := newRBGateHandler(t, model, []string{"openai", "anthropic"}, key, routeboundary.Boundary{
		APIKey:           key,
		Enabled:          true,
		AllowedProviders: []string{"openai", "anthropic"},
	})
	_ = recorder

	ctx := rbGateContext(ginCtx)
	// A provider object that only narrows to an in-boundary provider: the pool
	// stays usable and the named provider serves.
	body := `{"model":"` + model + `","provider":{"only":["anthropic"],"sort":"price","max_price":{"prompt":1}}}`
	_, _, errMsg := handler.ExecuteWithAuthManager(ctx, "openai", model, []byte(body), "")
	if errMsg != nil {
		t.Fatalf("in-boundary preference violated: %+v", errMsg)
	}
	if got := executors["anthropic"].CallCount(); got == 0 {
		t.Fatalf("anthropic executor not invoked; only:[anthropic] should pin it")
	}
}

// A request body with no provider object at all must route exactly as the
// pre-S2 build: no violation, no header change.
func TestProviderPrefsAbsentIsNoOp(t *testing.T) {
	const model = "pp-model-inbound"
	const key = "sk-pp-inbound"
	handler, executors, recorder, ginCtx := newRBGateHandler(t, model, []string{"openai", "anthropic"}, key, routeboundary.Boundary{
		APIKey:           key,
		Enabled:          true,
		AllowedProviders: []string{"openai", "anthropic"},
	})
	_ = recorder
	_ = executors

	ctx := rbGateContext(ginCtx)
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
	_, _, errMsg := handler.ExecuteWithAuthManager(ctx, "openai", model, []byte(body), "")
	if errMsg != nil {
		t.Fatalf("no-provider-object request must not error: %+v", errMsg)
	}
}

func strings_Contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}

var _ = handlers.NewBaseAPIHandlers
var _ gin.Context
