// Package config 定义 SSH 会话管理工具的数据模型、配置加载/保存/校验/CRUD。
//
// 数据模型三概念正交：Proxy（传输层代理）/ Jumphost（SSH 跳板）/ SSHServer（目标机）。
// Jumphost 用 SSHJ bool 字段区分两种形态：true = 透明转发（ssh -J 语义），
// false = 交互式堡垒机菜单。详见 docs/ssh-session-manager-design.md。
package config

import (
	"encoding/json"
	"fmt"
)

// ProxyType 表示传输层代理类型。用 string 而非 int + iota 以提高 JSON 可读性。
type ProxyType string

const (
	ProxyHTTP   ProxyType = "HTTP"
	ProxySOCKS5 ProxyType = "SOCKS5"
)

// ProxyAuth 是代理自身的认证信息（可选）。
type ProxyAuth struct {
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
}

// Proxy 是传输层代理（HTTP / SOCKS5），不含 SSH 跳板。
type Proxy struct {
	Name string     `json:"name"`
	Type ProxyType  `json:"type"`
	Addr string     `json:"addr"` // host:port
	Auth *ProxyAuth `json:"auth,omitempty"`
	Tags []string   `json:"tags,omitempty"`
}

// SSHAuth 是 SSH 认证信息，复用于 Jumphost 和 SSHServer。
// v1 仅支持 Password + PrivateKey 两种 SSH 认证方法。
type SSHAuth struct {
	Password   string `json:"password,omitempty"`
	PrivateKey string `json:"private_key,omitempty"` // 私钥文件完整路径（PEM）
	Passphrase string `json:"passphrase,omitempty"`  // 私钥口令，可空（空 = 私钥未加密）
}

// Expect 是 LoginAction 的一个分支：Pattern 命中后跳转到 Next 指向的 Action。
type Expect struct {
	Pattern string `json:"pattern"` // 无前缀 = glob，"re:" 前缀 = 正则
	Next    string `json:"next"`    // 另一个 LoginAction.Name，或 "success" 表示成功
}

// LoginAction 是决策树节点：一条 Send + 多个 Expects（按顺序尝试匹配）。
type LoginAction struct {
	Name      string   `json:"name"`
	Send      string   `json:"send,omitempty"` // 直接字符串，支持转义 \n \r \t；不支持变量引用
	Expects   []Expect `json:"expects"`
	TimeoutMs int      `json:"timeout_ms,omitempty"` // 0 = 默认 10000
}

// Jumphost 是 SSH 跳板。SSHJ 字段决定形态：
//   - true = 透明转发（ssh -J 语义），LoginFlow 必须为空
//   - false = 交互式堡垒机菜单，LoginFlow 必填，准备 jumphost 自身到主菜单就绪
//
// Via / Proxy 在 struct 里是指针，JSON 序列化为 name 字符串引用，
// 反序列化后由 Config.resolveReferences 解析回指针。
type Jumphost struct {
	Name            string                 `json:"name"`
	Addr            string                 `json:"addr"` // host:port
	User            string                 `json:"user"`
	Auth            SSHAuth                `json:"auth"`
	SSHJ            bool                   `json:"ssh_j"`
	LoginFlow       map[string]LoginAction `json:"login_flow,omitempty"`
	LoginEntry      string                 `json:"login_entry,omitempty"`
	MaxSteps        int                    `json:"max_steps,omitempty"`         // 0 = 默认 50
	GlobalTimeoutMs int                    `json:"global_timeout_ms,omitempty"` // 0 = 默认 60000
	Via             *Jumphost              `json:"-"`                           // 多跳递归口子（v1 不实现）
	Proxy           *Proxy                 `json:"-"`
	Tags            []string               `json:"tags,omitempty"`

	// 内部字段：UnmarshalJSON 时存原始 name 字符串，resolveReferences 时解析成 Via/Proxy 指针。
	viaName   string
	proxyName string
}

// SSHServer 是目标机。
//   - Pattern B（Via.SSHJ=false）下 LoginFlow 必填，从 jumphost 主菜单登录到 target；Auth 必须为空。
//   - Pattern A（Via 为空或 Via.SSHJ=true）下 LoginFlow 可选，承担 target 认证后交互；Auth 必填。
type SSHServer struct {
	Name            string                 `json:"name"`
	Addr            string                 `json:"addr"` // host:port
	User            string                 `json:"user"`
	Auth            SSHAuth                `json:"auth"`
	LoginFlow       map[string]LoginAction `json:"login_flow,omitempty"`
	LoginEntry      string                 `json:"login_entry,omitempty"`
	MaxSteps        int                    `json:"max_steps,omitempty"`
	GlobalTimeoutMs int                    `json:"global_timeout_ms,omitempty"`
	Via             *Jumphost              `json:"-"` // 可空，空表示直连
	Proxy           *Proxy                 `json:"-"` // 可空，空表示不走传输代理
	Tags            []string               `json:"tags,omitempty"`

	viaName   string
	proxyName string
}

// Config 是 config.json 的顶层结构。
type Config struct {
	Version      string       `json:"version"`
	IdleTimeoutS int          `json:"idle_timeout_s"` // 0 = 默认 300
	Jumphosts    []*Jumphost  `json:"jumphosts"`
	Proxies      []*Proxy     `json:"proxies"`
	Servers      []*SSHServer `json:"servers"`
}

// jumphostJSON 是 Jumphost 的 JSON 中间表示，把 Via/Proxy 指针序列化为 name 字符串。
type jumphostJSON struct {
	Name            string                 `json:"name"`
	Addr            string                 `json:"addr"`
	User            string                 `json:"user"`
	Auth            SSHAuth                `json:"auth"`
	SSHJ            bool                   `json:"ssh_j"`
	LoginFlow       map[string]LoginAction `json:"login_flow,omitempty"`
	LoginEntry      string                 `json:"login_entry,omitempty"`
	MaxSteps        int                    `json:"max_steps,omitempty"`
	GlobalTimeoutMs int                    `json:"global_timeout_ms,omitempty"`
	Via             string                 `json:"via,omitempty"`
	Proxy           string                 `json:"proxy,omitempty"`
	Tags            []string               `json:"tags,omitempty"`
}

func (j Jumphost) MarshalJSON() ([]byte, error) {
	jj := jumphostJSON{
		Name:            j.Name,
		Addr:            j.Addr,
		User:            j.User,
		Auth:            j.Auth,
		SSHJ:            j.SSHJ,
		LoginFlow:       j.LoginFlow,
		LoginEntry:      j.LoginEntry,
		MaxSteps:        j.MaxSteps,
		GlobalTimeoutMs: j.GlobalTimeoutMs,
		Tags:            j.Tags,
	}
	if j.Via != nil {
		jj.Via = j.Via.Name
	}
	if j.Proxy != nil {
		jj.Proxy = j.Proxy.Name
	}
	return json.Marshal(jj)
}

func (j *Jumphost) UnmarshalJSON(data []byte) error {
	var jj jumphostJSON
	if err := json.Unmarshal(data, &jj); err != nil {
		return err
	}
	j.Name = jj.Name
	j.Addr = jj.Addr
	j.User = jj.User
	j.Auth = jj.Auth
	j.SSHJ = jj.SSHJ
	j.LoginFlow = jj.LoginFlow
	j.LoginEntry = jj.LoginEntry
	j.MaxSteps = jj.MaxSteps
	j.GlobalTimeoutMs = jj.GlobalTimeoutMs
	j.Tags = jj.Tags
	j.viaName = jj.Via
	j.proxyName = jj.Proxy
	return nil
}

// serverJSON 是 SSHServer 的 JSON 中间表示。
type serverJSON struct {
	Name            string                 `json:"name"`
	Addr            string                 `json:"addr"`
	User            string                 `json:"user"`
	Auth            SSHAuth                `json:"auth"`
	LoginFlow       map[string]LoginAction `json:"login_flow,omitempty"`
	LoginEntry      string                 `json:"login_entry,omitempty"`
	MaxSteps        int                    `json:"max_steps,omitempty"`
	GlobalTimeoutMs int                    `json:"global_timeout_ms,omitempty"`
	Via             string                 `json:"via,omitempty"`
	Proxy           string                 `json:"proxy,omitempty"`
	Tags            []string               `json:"tags,omitempty"`
}

func (s SSHServer) MarshalJSON() ([]byte, error) {
	sj := serverJSON{
		Name:            s.Name,
		Addr:            s.Addr,
		User:            s.User,
		Auth:            s.Auth,
		LoginFlow:       s.LoginFlow,
		LoginEntry:      s.LoginEntry,
		MaxSteps:        s.MaxSteps,
		GlobalTimeoutMs: s.GlobalTimeoutMs,
		Tags:            s.Tags,
	}
	if s.Via != nil {
		sj.Via = s.Via.Name
	}
	if s.Proxy != nil {
		sj.Proxy = s.Proxy.Name
	}
	return json.Marshal(sj)
}

func (s *SSHServer) UnmarshalJSON(data []byte) error {
	var sj serverJSON
	if err := json.Unmarshal(data, &sj); err != nil {
		return err
	}
	s.Name = sj.Name
	s.Addr = sj.Addr
	s.User = sj.User
	s.Auth = sj.Auth
	s.LoginFlow = sj.LoginFlow
	s.LoginEntry = sj.LoginEntry
	s.MaxSteps = sj.MaxSteps
	s.GlobalTimeoutMs = sj.GlobalTimeoutMs
	s.Tags = sj.Tags
	s.viaName = sj.Via
	s.proxyName = sj.Proxy
	return nil
}

// resolveReferences 把所有 Jumphost/SSHServer 的 viaName/proxyName 解析成 Via/Proxy 指针。
// 必须在 UnmarshalJSON 之后调用。重复 name 或引用不存在时返回错误。
func (c *Config) resolveReferences() error {
	jumphostByName := make(map[string]*Jumphost)
	for _, j := range c.Jumphosts {
		if _, exists := jumphostByName[j.Name]; exists {
			return fmt.Errorf("duplicate jumphost name: %q", j.Name)
		}
		jumphostByName[j.Name] = j
	}
	proxyByName := make(map[string]*Proxy)
	for _, p := range c.Proxies {
		if _, exists := proxyByName[p.Name]; exists {
			return fmt.Errorf("duplicate proxy name: %q", p.Name)
		}
		proxyByName[p.Name] = p
	}
	for _, j := range c.Jumphosts {
		if j.viaName != "" {
			ref, ok := jumphostByName[j.viaName]
			if !ok {
				return fmt.Errorf("jumphost %q references unknown jumphost %q", j.Name, j.viaName)
			}
			j.Via = ref
		}
		if j.proxyName != "" {
			ref, ok := proxyByName[j.proxyName]
			if !ok {
				return fmt.Errorf("jumphost %q references unknown proxy %q", j.Name, j.proxyName)
			}
			j.Proxy = ref
		}
	}
	for _, s := range c.Servers {
		if s.viaName != "" {
			ref, ok := jumphostByName[s.viaName]
			if !ok {
				return fmt.Errorf("server %q references unknown jumphost %q", s.Name, s.viaName)
			}
			s.Via = ref
		}
		if s.proxyName != "" {
			ref, ok := proxyByName[s.proxyName]
			if !ok {
				return fmt.Errorf("server %q references unknown proxy %q", s.Name, s.proxyName)
			}
			s.Proxy = ref
		}
	}
	return nil
}
