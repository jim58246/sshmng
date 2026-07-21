package config

import (
	"strings"
	"testing"
)

func validAction(next string) LoginAction {
	return LoginAction{Expects: []Expect{{Pattern: "x", Next: next}}}
}

func TestValidateJumphostSSHJTrueRejectsLoginFlow(t *testing.T) {
	jh := &Jumphost{
		Name:      "jh",
		Addr:      "h:22",
		User:      "u",
		Auth:      SSHAuth{Password: "p"},
		SSHJ:      true,
		LoginFlow: map[string]LoginAction{"a": validAction("success")},
	}
	cfg := &Config{Version: "1", Jumphosts: []*Jumphost{jh}}
	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected error: SSHJ=true with non-empty LoginFlow")
	}
	if !strings.Contains(err.Error(), "ssh_j=true") {
		t.Errorf("error should mention ssh_j=true, got: %v", err)
	}
}

func TestValidateJumphostSSHJTrueRejectsLoginEntry(t *testing.T) {
	jh := &Jumphost{
		Name:       "jh",
		Addr:       "h:22",
		User:       "u",
		Auth:       SSHAuth{Password: "p"},
		SSHJ:       true,
		LoginEntry: "a",
	}
	cfg := &Config{Version: "1", Jumphosts: []*Jumphost{jh}}
	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected error: SSHJ=true with non-empty LoginEntry")
	}
}

func TestValidateJumphostSSHJFalseRequiresLoginFlow(t *testing.T) {
	jh := &Jumphost{
		Name:       "jh",
		Addr:       "h:22",
		User:       "u",
		Auth:       SSHAuth{Password: "p"},
		SSHJ:       false,
		LoginFlow:  nil,
		LoginEntry: "a",
	}
	cfg := &Config{Version: "1", Jumphosts: []*Jumphost{jh}}
	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected error: SSHJ=false with empty LoginFlow")
	}
}

func TestValidateJumphostSSHJFalseRejectsUnknownLoginEntry(t *testing.T) {
	jh := &Jumphost{
		Name:       "jh",
		Addr:       "h:22",
		User:       "u",
		Auth:       SSHAuth{Password: "p"},
		SSHJ:       false,
		LoginFlow:  map[string]LoginAction{"a": validAction("success")},
		LoginEntry: "no-such-action",
	}
	cfg := &Config{Version: "1", Jumphosts: []*Jumphost{jh}}
	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected error: LoginEntry not in LoginFlow")
	}
}

func TestValidateJumphostSSHJFalseAcceptsValid(t *testing.T) {
	jh := &Jumphost{
		Name:       "jh",
		Addr:       "h:22",
		User:       "u",
		Auth:       SSHAuth{Password: "p"},
		SSHJ:       false,
		LoginFlow:  map[string]LoginAction{"a": validAction("success")},
		LoginEntry: "a",
	}
	cfg := &Config{Version: "1", Jumphosts: []*Jumphost{jh}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateSSHServerPatternBRequiresLoginFlow(t *testing.T) {
	jh := &Jumphost{Name: "jh", Addr: "h:22", User: "u", Auth: SSHAuth{Password: "p"}, SSHJ: false,
		LoginFlow: map[string]LoginAction{"a": validAction("success")}, LoginEntry: "a"}
	srv := &SSHServer{Name: "s", Addr: "t:22", User: "u", Auth: SSHAuth{}, Via: jh}
	cfg := &Config{Version: "1", Jumphosts: []*Jumphost{jh}, Servers: []*SSHServer{srv}}
	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected error: Pattern B SSHServer must have LoginFlow")
	}
}

func TestValidateSSHServerPatternBRejectsAuth(t *testing.T) {
	jh := &Jumphost{Name: "jh", Addr: "h:22", User: "u", Auth: SSHAuth{Password: "p"}, SSHJ: false,
		LoginFlow: map[string]LoginAction{"a": validAction("success")}, LoginEntry: "a"}
	srv := &SSHServer{
		Name: "s", Addr: "t:22", User: "u",
		Auth:       SSHAuth{Password: "should-be-empty"},
		Via:        jh,
		LoginFlow:  map[string]LoginAction{"a": validAction("success")},
		LoginEntry: "a",
	}
	cfg := &Config{Version: "1", Jumphosts: []*Jumphost{jh}, Servers: []*SSHServer{srv}}
	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected error: Pattern B SSHServer Auth must be empty")
	}
}

func TestValidateSSHServerPatternBAcceptsValid(t *testing.T) {
	jh := &Jumphost{Name: "jh", Addr: "h:22", User: "u", Auth: SSHAuth{Password: "p"}, SSHJ: false,
		LoginFlow: map[string]LoginAction{"a": validAction("success")}, LoginEntry: "a"}
	srv := &SSHServer{
		Name: "s", Addr: "t:22", User: "u", Auth: SSHAuth{},
		Via:        jh,
		LoginFlow:  map[string]LoginAction{"a": validAction("success")},
		LoginEntry: "a",
	}
	cfg := &Config{Version: "1", Jumphosts: []*Jumphost{jh}, Servers: []*SSHServer{srv}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateSSHServerPatternAOptionalLoginFlow(t *testing.T) {
	// 直连 server，无 LoginFlow —— 合法
	srv := &SSHServer{Name: "s", Addr: "t:22", User: "u", Auth: SSHAuth{Password: "p"}}
	cfg := &Config{Version: "1", Servers: []*SSHServer{srv}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateSSHServerPatternAWithLoginFlowRequiresEntry(t *testing.T) {
	// 直连 server，LoginFlow 非空但 LoginEntry 缺失 —— 报错
	srv := &SSHServer{
		Name: "s", Addr: "t:22", User: "u", Auth: SSHAuth{Password: "p"},
		LoginFlow: map[string]LoginAction{"a": validAction("success")},
	}
	cfg := &Config{Version: "1", Servers: []*SSHServer{srv}}
	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected error: LoginFlow non-empty but LoginEntry missing")
	}
}

func TestValidateSSHServerPatternAViaSSHJTrueAccepts(t *testing.T) {
	jh := &Jumphost{Name: "jh", Addr: "h:22", User: "u", Auth: SSHAuth{Password: "p"}, SSHJ: true}
	srv := &SSHServer{Name: "s", Addr: "t:22", User: "u", Auth: SSHAuth{Password: "p"}, Via: jh}
	cfg := &Config{Version: "1", Jumphosts: []*Jumphost{jh}, Servers: []*SSHServer{srv}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateRejectsSuccessAsLoginFlowKey(t *testing.T) {
	jh := &Jumphost{
		Name:       "jh",
		Addr:       "h:22",
		User:       "u",
		Auth:       SSHAuth{Password: "p"},
		SSHJ:       false,
		LoginFlow:  map[string]LoginAction{"success": validAction("success")},
		LoginEntry: "success",
	}
	cfg := &Config{Version: "1", Jumphosts: []*Jumphost{jh}}
	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected error: \"success\" as LoginFlow key")
	}
}

func TestValidateRejectsUnknownExpectNext(t *testing.T) {
	jh := &Jumphost{
		Name:       "jh",
		Addr:       "h:22",
		User:       "u",
		Auth:       SSHAuth{Password: "p"},
		SSHJ:       false,
		LoginFlow:  map[string]LoginAction{"a": validAction("no-such-action")},
		LoginEntry: "a",
	}
	cfg := &Config{Version: "1", Jumphosts: []*Jumphost{jh}}
	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected error: Expect.Next points to unknown action")
	}
}

func TestValidateRejectsEmptyExpects(t *testing.T) {
	jh := &Jumphost{
		Name:       "jh",
		Addr:       "h:22",
		User:       "u",
		Auth:       SSHAuth{Password: "p"},
		SSHJ:       false,
		LoginFlow:  map[string]LoginAction{"a": {Expects: nil}},
		LoginEntry: "a",
	}
	cfg := &Config{Version: "1", Jumphosts: []*Jumphost{jh}}
	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected error: LoginAction with empty Expects")
	}
}

func TestValidateRejectsJumphostMissingAuth(t *testing.T) {
	jh := &Jumphost{Name: "jh", Addr: "h:22", User: "u", SSHJ: true}
	cfg := &Config{Version: "1", Jumphosts: []*Jumphost{jh}}
	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected error: Jumphost missing Auth")
	}
}

func TestValidateRejectsSSHServerPatternAMissingAuth(t *testing.T) {
	srv := &SSHServer{Name: "s", Addr: "t:22", User: "u"} // 无 Auth
	cfg := &Config{Version: "1", Servers: []*SSHServer{srv}}
	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected error: Pattern A SSHServer missing Auth")
	}
}

func TestValidateAcceptsFullValidConfig(t *testing.T) {
	jh := &Jumphost{
		Name: "jh", Addr: "h:22", User: "u", Auth: SSHAuth{Password: "p"}, SSHJ: false,
		LoginFlow:  map[string]LoginAction{"a": validAction("success")},
		LoginEntry: "a",
	}
	srv1 := &SSHServer{
		Name: "s1", Addr: "t1:22", User: "u", Auth: SSHAuth{},
		Via:        jh,
		LoginFlow:  map[string]LoginAction{"a": validAction("success")},
		LoginEntry: "a",
	}
	srv2 := &SSHServer{
		Name: "s2", Addr: "t2:22", User: "u", Auth: SSHAuth{Password: "p"},
	}
	cfg := &Config{
		Version:      "1",
		IdleTimeoutS: 300,
		Jumphosts:    []*Jumphost{jh},
		Servers:      []*SSHServer{srv1, srv2},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// authIsEmpty 在 validate.go 中定义。

func TestValidateRejectsSSHServerPatternBWithPartialAuth(t *testing.T) {
	jh := &Jumphost{Name: "jh", Addr: "h:22", User: "u", Auth: SSHAuth{Password: "p"}, SSHJ: false,
		LoginFlow: map[string]LoginAction{"a": validAction("success")}, LoginEntry: "a"}
	// Pattern B + 仅 Password 非空，应拒绝
	srv := &SSHServer{
		Name: "s", Addr: "t:22", User: "u",
		Auth:       SSHAuth{Password: "leak"},
		Via:        jh,
		LoginFlow:  map[string]LoginAction{"a": validAction("success")},
		LoginEntry: "a",
	}
	cfg := &Config{Version: "1", Jumphosts: []*Jumphost{jh}, Servers: []*SSHServer{srv}}
	err := cfg.Validate()
	if err == nil {
		t.Fatalf("expected error: Pattern B with partial Auth")
	}
}

func TestValidateAuthHelper(t *testing.T) {
	if !authIsEmpty(SSHAuth{}) {
		t.Errorf("empty SSHAuth should be considered empty")
	}
	if authIsEmpty(SSHAuth{Password: "x"}) {
		t.Errorf("SSHAuth with Password should not be considered empty")
	}
}
