package server

import (
	"github.com/me/metaai2api/internal/meta/mock"
)

// newMockBackend returns the default deterministic backend. It is used for
// config.BackendMock; every other backend needs its own adapter here so a
// config reload can never point /v1 at the wrong implementation.
func newMockBackend() *mock.Backend { return mock.New() }
