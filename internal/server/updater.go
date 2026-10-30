package server

import (
	"context"

	"github.com/me/metaai2api/internal/updater"
)

// NewUpdaterFunc adapts an updater.Updater to the hook POST /admin/update
// runs, for wiring with SetUpdater. It exists here because updateResult is
// unexported and so cannot be built from the wiring package.
//
// The update runs detached from the HTTP request: replacing the binary is not
// something to abandon halfway because an admin client hung up, and the check
// is bounded by the updater's own HTTP client timeouts.
func NewUpdaterFunc(u *updater.Updater) UpdateFunc {
	if u == nil {
		return nil
	}
	return func() (updateResult, error) {
		res, err := u.Check(context.Background())
		if err != nil {
			return updateResult{}, err
		}
		out := updateResult{Applied: res.Applied, Version: res.Current}
		if res.Applied {
			out.Version = res.Version
		}
		return out, nil
	}
}

// NewUpdateChecker adapts an updater.Updater to the view /admin/health
// reports. It never contacts the network: it reports the most recent Check.
func NewUpdateChecker(u *updater.Updater) func() updateStatus {
	if u == nil {
		return nil
	}
	return func() updateStatus {
		res, at, _ := u.Last()
		st := updateStatus{
			Available: res.Available,
			Checked:   !at.IsZero(),
			Current:   res.Current,
			Latest:    res.Latest,
		}
		if !at.IsZero() {
			st.At = at.UTC().Format("2006-01-02T15:04:05Z07:00")
		}
		return st
	}
}
