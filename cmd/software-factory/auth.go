// Auth subcommands: Grok (native OAuth) + Claude (subscription proxy registration).
//
//	software-factory login [grok|xai|claude] …
//	software-factory logout [grok|xai|claude|all]
//	software-factory auth status
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/Loxstomper/software-factory/internal/auth"
)

func cmdLogin(args []string) error {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprint(os.Stdout, `software-factory login — sign in with a model subscription

usage:
  software-factory login [grok|xai]          SuperGrok / X Premium+ device-code OAuth
  software-factory login claude --proxy URL  Claude Pro/Max via local subscription proxy

Grok (default):
  Opens a verification URL from auth.x.ai. Approve in any browser; tokens are stored
  at ~/.software-factory/auth.json (mode 0600) and refreshed on use.

  Once logged in, point souls at Grok models (openai-compat, family xai) and leave
  OPENAI_API_KEY / XAI_API_KEY unset — the registry uses the OAuth Bearer.
  Prefer endpoint https://cli-chat-proxy.grok.com/v1 for subscription tokens
  (api.x.ai may return 402/403 for sub tokens).

Claude:
  Anthropic does not expose a public third-party device-code flow for Pro/Max.
  Run a local OpenAI-compatible or Anthropic-compatible proxy that uses your Claude
  subscription (e.g. a Claude Code bridge), then:

    software-factory login claude --proxy http://127.0.0.1:PORT/v1
    software-factory login claude --proxy http://127.0.0.1:PORT --mode anthropic
    software-factory login claude --proxy URL --token BEARER   # if the proxy needs a key

  Then point model registry entries at that endpoint (see docs/selecting-provider.md).

Both providers can be logged in at once (e.g. Grok implementor + Claude verifier).

`)
		return nil
	}

	provider := auth.ProviderXAI
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		provider = auth.NormalizeProvider(args[0])
		rest = args[1:]
	}

	switch provider {
	case auth.ProviderXAI:
		if len(rest) > 0 {
			return fmt.Errorf("login grok takes no flags (got %v); see login -h", rest)
		}
		_, err := auth.Login(os.Stdout)
		return err
	case auth.ProviderClaude:
		return loginClaude(rest)
	default:
		return fmt.Errorf("unknown login provider %q (want grok|xai|claude)", args[0])
	}
}

func loginClaude(args []string) error {
	var proxy, token, mode string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-h", "--help":
			return cmdLogin([]string{"-h"})
		case "--proxy":
			if i+1 >= len(args) {
				return fmt.Errorf("login claude: --proxy needs a URL")
			}
			i++
			proxy = args[i]
		case "--token":
			if i+1 >= len(args) {
				return fmt.Errorf("login claude: --token needs a value")
			}
			i++
			token = args[i]
		case "--mode":
			if i+1 >= len(args) {
				return fmt.Errorf("login claude: --mode needs openai-compat|anthropic")
			}
			i++
			mode = args[i]
		default:
			return fmt.Errorf("login claude: unknown flag %q", args[i])
		}
	}
	if proxy == "" {
		return fmt.Errorf("login claude: --proxy URL is required (see login -h)")
	}
	return auth.LoginClaude(os.Stdout, proxy, token, mode)
}

func cmdLogout(args []string) error {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprint(os.Stdout, `software-factory logout — clear stored subscription credentials

usage:
  software-factory logout              clear Grok / xAI (default)
  software-factory logout grok|xai
  software-factory logout claude
  software-factory logout all

`)
		return nil
	}
	provider := auth.ProviderXAI
	if len(args) > 0 {
		provider = args[0]
	}
	if err := auth.Clear(provider); err != nil {
		return err
	}
	switch auth.NormalizeProvider(provider) {
	case auth.ProviderXAI:
		fmt.Fprintln(os.Stdout, "Logged out of Grok / xAI.")
	case auth.ProviderClaude:
		fmt.Fprintln(os.Stdout, "Cleared Claude subscription proxy.")
	case "all":
		fmt.Fprintln(os.Stdout, "Cleared all software-factory auth credentials.")
	}
	return nil
}

func cmdAuth(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("auth: missing subcommand (want status); see auth -h")
	}
	if args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(os.Stdout, `software-factory auth — inspect subscription credentials

usage:
  software-factory auth status

`)
		return nil
	}
	switch args[0] {
	case "status":
		return cmdAuthStatus(args[1:])
	default:
		return fmt.Errorf("auth: unknown subcommand %q (want status)", args[0])
	}
}

func cmdAuthStatus(args []string) error {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprint(os.Stdout, "software-factory auth status — show Grok and Claude subscription auth\n")
		return nil
	}
	s, err := auth.Status()
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, s)
	return nil
}
