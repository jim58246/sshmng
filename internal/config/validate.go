package config

import (
	"fmt"
)

// successSentinel 是 LoginFlow 中表示登录成功的保留字符串，不能作为 key。
const successSentinel = "success"

// authIsEmpty 判断 SSHAuth 是否全空（无任何认证信息）。
func authIsEmpty(a SSHAuth) bool {
	return a.Password == "" && a.PrivateKey == "" && a.Passphrase == ""
}

// Validate 校验配置内容：SSHJ/LoginFlow 一致性、Auth 必空规则、"success" 保留字、
// LoginEntry 与 Expect.Next 引用合法性、LoginAction.Expects 非空。
// 引用完整性（via/proxy name 是否存在）由 resolveReferences 在加载时校验，
// 但 CRUD 修改后若新增悬空引用，也应在此处补检（此处假设 Via/Proxy 指针已解析）。
func (c *Config) Validate() error {
	for _, j := range c.Jumphosts {
		if err := validateJumphost(j); err != nil {
			return fmt.Errorf("jumphost %q: %w", j.Name, err)
		}
	}
	for _, s := range c.Servers {
		if err := validateSSHServer(s); err != nil {
			return fmt.Errorf("server %q: %w", s.Name, err)
		}
	}
	return nil
}

func validateJumphost(j *Jumphost) error {
	if authIsEmpty(j.Auth) {
		return fmt.Errorf("auth is required")
	}
	if j.SSHJ {
		if len(j.LoginFlow) > 0 {
			return fmt.Errorf("ssh_j=true requires empty login_flow")
		}
		if j.LoginEntry != "" {
			return fmt.Errorf("ssh_j=true requires empty login_entry")
		}
		return nil
	}
	// SSHJ=false：交互式堡垒机
	if len(j.LoginFlow) == 0 {
		return fmt.Errorf("ssh_j=false requires non-empty login_flow")
	}
	if err := validateLoginFlow(j.LoginFlow, j.LoginEntry); err != nil {
		return err
	}
	return nil
}

func validateSSHServer(s *SSHServer) error {
	patternB := s.Via != nil && !s.Via.SSHJ
	if patternB {
		if !authIsEmpty(s.Auth) {
			return fmt.Errorf("pattern B (via.ssh_j=false) requires empty auth; put credentials in login_flow.send")
		}
		if len(s.LoginFlow) == 0 {
			return fmt.Errorf("pattern B (via.ssh_j=false) requires non-empty login_flow to log in to target")
		}
		if err := validateLoginFlow(s.LoginFlow, s.LoginEntry); err != nil {
			return err
		}
		return nil
	}
	// Pattern A（直连或 Via.SSHJ=true）
	if authIsEmpty(s.Auth) {
		return fmt.Errorf("pattern A requires auth (used for SSH auth to target)")
	}
	if len(s.LoginFlow) > 0 {
		if err := validateLoginFlow(s.LoginFlow, s.LoginEntry); err != nil {
			return err
		}
	}
	return nil
}

// validateLoginFlow 校验 LoginFlow 决策树：
//   - "success" 不能作为 key
//   - LoginEntry 必须指向 Flow 中存在的 Action
//   - 每个 LoginAction.Expects 至少一条 pattern
//   - 每个 Expect.Next 必须是 "success" 或 Flow 中存在的 Action name
func validateLoginFlow(flow map[string]LoginAction, entry string) error {
	if _, ok := flow[successSentinel]; ok {
		return fmt.Errorf("%q is reserved, cannot be used as login_flow key", successSentinel)
	}
	if entry == "" {
		return fmt.Errorf("login_entry is required when login_flow is non-empty")
	}
	if _, ok := flow[entry]; !ok {
		return fmt.Errorf("login_entry %q not found in login_flow", entry)
	}
	for name, action := range flow {
		if len(action.Expects) == 0 {
			return fmt.Errorf("login_flow[%q]: expects must have at least one pattern", name)
		}
		for i, exp := range action.Expects {
			if exp.Pattern == "" {
				return fmt.Errorf("login_flow[%q].expects[%d]: pattern is empty", name, i)
			}
			if exp.Next == "" {
				return fmt.Errorf("login_flow[%q].expects[%d]: next is empty", name, i)
			}
			if exp.Next == successSentinel {
				continue
			}
			if _, ok := flow[exp.Next]; !ok {
				return fmt.Errorf("login_flow[%q].expects[%d].next %q not found in login_flow", name, i, exp.Next)
			}
		}
	}
	return nil
}
