// Auth subcommands: native Grok / SuperGrok / X Premium+ OAuth (device-code).
//
// Easy names:
//
//	software-factory login
//	software-factory logout
//	software-factory auth status
package main

import (
	"fmt"
	"os"

	"github.com/Loxstomper/software-factory/internal/auth"
)

func cmdLogin(args []string) error {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprint(os.Stdout, `software-factory login — sign in with SuperGrok / X Premium+ (device-code OAuth)

Opens (or prints) a verification URL from auth.x.ai. Approve in any browser;
tokens are stored at ~/.software-factory/auth.json (mode 0600) and refreshed
automatically on subsequent runs.

Once logged in, point souls at Grok models (openai-compat) and leave
OPENAI_API_KEY / XAI_API_KEY unset — the registry uses the OAuth Bearer.

For pure subscription use prefer endpoint https://cli-chat-proxy.grok.com/v1
(see docs/selecting-provider.md); api.x.ai may return 402/403 tier gates for
subscription tokens.

`)
		return nil
	}
	_, err := auth.Login(os.Stdout)
	return err
}

func cmdLogout(args []string) error {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprint(os.Stdout, "software-factory logout — clear stored xAI / Grok OAuth tokens\n")
		return nil
	}
	if err := auth.Clear(); err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "Logged out. ~/.software-factory/auth.json xAI entry cleared.")
	return nil
}

func cmdAuthStatus(args []string) error {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprint(os.Stdout, "software-factory auth status — show current Grok / xAI OAuth status\n")
		return nil
	}
	s, err := auth.Status()
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, s)
	return nil
}
