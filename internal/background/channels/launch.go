package channels

import "github.com/mbathepaul/digitorn/internal/background/daemonclient"

// ToLaunchSpec maps a resolved Activation onto a daemon-client launch for the
// given app. The per-session entry agent + extra context ride the createSession
// passthrough fields (honored by the daemon's session meta). per_event's empty
// session id is preserved so the processor derives a deterministic, idempotent
// per-delivery id. reply:auto requests the reply be read back.
func (a Activation) ToLaunchSpec(appID string) daemonclient.LaunchSpec {
	return daemonclient.LaunchSpec{
		AppID:        appID,
		SessionID:    a.Session,
		OwnerUserID:  a.Owner,
		Message:      a.Message,
		EntryAgent:   a.Agent,
		Context:      a.Context,
		WaitForReply: a.Reply == ReplyAuto,
	}
}
