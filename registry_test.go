//go:build ignore
// +build ignore

package register

import (
	"os"
	"testing"

	"go.unistack.org/micro/v3/register/memory"
	"go.unistack.org/micro/v3/router"
)

func routerTestSetup() router.Router {
	r := memory.NewRegister()
	return NewRouter(router.Register(r))
}

func TestRouterClose(t *testing.T) {
	r := routerTestSetup()

	if err := r.Close(); err != nil {
		t.Errorf("failed to stop router: %v", err)
	}
	if len(os.Getenv("INTEGRATION_TESTS")) == 0 {
		t.Logf("TestRouterStartStop STOPPED")
	}
}
