package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/pflag"

	"github.com/jim58246/sshmng/internal/config"
)

// entityType identifies which config entity type a subcommand operates on.
type entityType string

const (
	entityServer   entityType = "server"
	entityJumphost entityType = "jumphost"
	entityProxy    entityType = "proxy"
)

// runEntityCmd dispatches server/jumphost/proxy subcommands.
func runEntityCmd(et entityType, args []string, out io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(out, "Usage: sshmng %s <list|get> [...]\n", et)
		return 2
	}
	switch args[0] {
	case "list":
		return runEntityList(et, args[1:], out)
	case "get":
		return runEntityGet(et, args[1:], out)
	default:
		fmt.Fprintf(out, "Unknown %s subcommand %q. Use 'list' or 'get'.\n", et, args[0])
		return 2
	}
}

// runServerCmd is the Dispatch entry point for 'sshmng server'.
func runServerCmd(_ interface{}, args []string, out io.Writer) int {
	return runEntityCmd(entityServer, args, out)
}

// runJumphostCmd is the Dispatch entry point for 'sshmng jumphost'.
func runJumphostCmd(_ interface{}, args []string, out io.Writer) int {
	return runEntityCmd(entityJumphost, args, out)
}

// runProxyCmd is the Dispatch entry point for 'sshmng proxy'.
func runProxyCmd(_ interface{}, args []string, out io.Writer) int {
	return runEntityCmd(entityProxy, args, out)
}

func runEntityList(et entityType, args []string, out io.Writer) int {
	fs := pflag.NewFlagSet(string(et)+" list", pflag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fmt.Fprintf(out, "Usage: sshmng %s list [keywords...] [--config <path>]\n", et)
		fmt.Fprintf(out, "  Multiple positional args are AND-matched against name/addr/tags.\n")
		fs.PrintDefaults()
	}
	configPath := fs.String("config", "", "path to config.json")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	query := strings.Join(fs.Args(), " ")

	_, cfg, err := BootstrapConfig(*configPath)
	if err != nil {
		fmt.Fprintf(out, "Error: %v\n", err)
		return 1
	}

	switch et {
	case entityServer:
		printServerList(cfg.ListSSHServers(query), out)
	case entityJumphost:
		printJumphostList(cfg.ListJumphosts(query), out)
	case entityProxy:
		printProxyList(cfg.ListProxies(query), out)
	}
	return 0
}

func runEntityGet(et entityType, args []string, out io.Writer) int {
	fs := pflag.NewFlagSet(string(et)+" get", pflag.ContinueOnError)
	fs.SetOutput(out)
	configPath := fs.String("config", "", "path to config.json")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if fs.NArg() != 1 {
		fmt.Fprintf(out, "Usage: sshmng %s get <name>\n", et)
		if fs.NArg() > 1 {
			fmt.Fprintf(out, "Error: expected exactly one name, got %d (%v)\n", fs.NArg(), fs.Args())
		}
		return 2
	}
	name := fs.Arg(0)

	_, cfg, err := BootstrapConfig(*configPath)
	if err != nil {
		fmt.Fprintf(out, "Error: %v\n", err)
		return 1
	}

	switch et {
	case entityServer:
		srv, err := cfg.GetSSHServer(name)
		if err != nil {
			fmt.Fprintf(out, "Error: %v\n", err)
			return 1
		}
		printServerDetail(srv, out)
	case entityJumphost:
		j, err := cfg.GetJumphost(name)
		if err != nil {
			fmt.Fprintf(out, "Error: %v\n", err)
			return 1
		}
		printJumphostDetail(j, out)
	case entityProxy:
		p, err := cfg.GetProxy(name)
		if err != nil {
			fmt.Fprintf(out, "Error: %v\n", err)
			return 1
		}
		printProxyDetail(p, out)
	}
	return 0
}

// --- List formatters (table) ---

func printServerList(servers []*config.SSHServer, out io.Writer) {
	if len(servers) == 0 {
		fmt.Fprintln(out, "No servers found.")
		return
	}
	fmt.Fprintf(out, "%-20s %-25s %-15s %-15s %s\n", "NAME", "ADDR", "VIA", "PROXY", "TAGS")
	for _, s := range servers {
		via := "-"
		if s.Via != nil {
			via = s.Via.Name
		}
		proxy := "-"
		if s.Proxy != nil {
			proxy = s.Proxy.Name
		}
		tags := strings.Join(s.Tags, ",")
		fmt.Fprintf(out, "%-20s %-25s %-15s %-15s %s\n", s.Name, s.Addr, via, proxy, tags)
	}
}

func printJumphostList(items []*config.Jumphost, out io.Writer) {
	if len(items) == 0 {
		fmt.Fprintln(out, "No jumphosts found.")
		return
	}
	fmt.Fprintf(out, "%-20s %-25s %-10s %-15s %-15s %s\n", "NAME", "ADDR", "SSH_J", "VIA", "PROXY", "TAGS")
	for _, j := range items {
		via := "-"
		if j.Via != nil {
			via = j.Via.Name
		}
		proxy := "-"
		if j.Proxy != nil {
			proxy = j.Proxy.Name
		}
		tags := strings.Join(j.Tags, ",")
		fmt.Fprintf(out, "%-20s %-25s %-10t %-15s %-15s %s\n", j.Name, j.Addr, j.SSHJ, via, proxy, tags)
	}
}

func printProxyList(items []*config.Proxy, out io.Writer) {
	if len(items) == 0 {
		fmt.Fprintln(out, "No proxies found.")
		return
	}
	fmt.Fprintf(out, "%-20s %-10s %-25s %-6s %s\n", "NAME", "TYPE", "ADDR", "AUTH", "TAGS")
	for _, p := range items {
		auth := "-"
		if p.Auth != nil && (p.Auth.User != "" || p.Auth.Password != "") {
			auth = "yes"
		}
		tags := strings.Join(p.Tags, ",")
		fmt.Fprintf(out, "%-20s %-10s %-25s %-6s %s\n", p.Name, p.Type, p.Addr, auth, tags)
	}
}

// --- Detail formatters (key-value) ---

func printServerDetail(s *config.SSHServer, out io.Writer) {
	fmt.Fprintf(out, "Name:           %s\n", s.Name)
	fmt.Fprintf(out, "Addr:           %s\n", s.Addr)
	fmt.Fprintf(out, "User:           %s\n", s.User)
	fmt.Fprintf(out, "Auth:           %s\n", redactAuth(s.Auth))
	via := "-"
	if s.Via != nil {
		via = s.Via.Name
	}
	fmt.Fprintf(out, "Via:            %s\n", via)
	proxy := "-"
	if s.Proxy != nil {
		proxy = s.Proxy.Name
	}
	fmt.Fprintf(out, "Proxy:          %s\n", proxy)
	fmt.Fprintf(out, "Tags:           %s\n", strings.Join(s.Tags, ", "))
	fmt.Fprintf(out, "HostKeyVerify:  %s\n", boolDesc(s.HostKeyVerify))
	fmt.Fprintf(out, "LoginFlow:      %s\n", flowDesc(len(s.LoginFlow), s.LoginEntry))
}

func printJumphostDetail(j *config.Jumphost, out io.Writer) {
	fmt.Fprintf(out, "Name:           %s\n", j.Name)
	fmt.Fprintf(out, "Addr:           %s\n", j.Addr)
	fmt.Fprintf(out, "User:           %s\n", j.User)
	fmt.Fprintf(out, "Auth:           %s\n", redactAuth(j.Auth))
	fmt.Fprintf(out, "SSH_J:          %t\n", j.SSHJ)
	via := "-"
	if j.Via != nil {
		via = j.Via.Name
	}
	fmt.Fprintf(out, "Via:            %s\n", via)
	proxy := "-"
	if j.Proxy != nil {
		proxy = j.Proxy.Name
	}
	fmt.Fprintf(out, "Proxy:          %s\n", proxy)
	fmt.Fprintf(out, "Tags:           %s\n", strings.Join(j.Tags, ", "))
	fmt.Fprintf(out, "HostKeyVerify:  %s\n", boolDesc(j.HostKeyVerify))
	fmt.Fprintf(out, "LoginFlow:      %s\n", flowDesc(len(j.LoginFlow), j.LoginEntry))
}

func printProxyDetail(p *config.Proxy, out io.Writer) {
	fmt.Fprintf(out, "Name:           %s\n", p.Name)
	fmt.Fprintf(out, "Type:           %s\n", p.Type)
	fmt.Fprintf(out, "Addr:           %s\n", p.Addr)
	if p.Auth != nil && p.Auth.User != "" {
		fmt.Fprintf(out, "Auth.User:      %s\n", p.Auth.User)
		fmt.Fprintf(out, "Auth.Password:  ***\n")
	}
	fmt.Fprintf(out, "Tags:           %s\n", strings.Join(p.Tags, ", "))
}

// --- helpers ---

func redactAuth(a config.SSHAuth) string {
	parts := []string{}
	if a.Password != "" {
		parts = append(parts, "password=***")
	}
	if a.PrivateKey != "" {
		parts = append(parts, "private_key=***")
	}
	if a.Passphrase != "" {
		parts = append(parts, "passphrase=***")
	}
	if len(parts) == 0 {
		return "(none)"
	}
	return strings.Join(parts, ", ")
}

func boolDesc(p *bool) string {
	if p == nil {
		return "true (default)"
	}
	return fmt.Sprintf("%t", *p)
}

func flowDesc(flowLen int, entry string) string {
	if flowLen == 0 {
		return "(none)"
	}
	return fmt.Sprintf("%d actions, entry=%q", flowLen, entry)
}
