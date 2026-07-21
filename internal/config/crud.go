package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ListSSHServers 返回匹配 query 的 server 列表。query 为空（或纯空白）返回全部。
// query 按空白分词为多关键字，AND 语义：每个关键字都需命中 name / addr / 任一 tag
// （大小写不敏感的子串匹配）。例：query="prod web" 只返回同时含 "prod" 和 "web" 的 server。
func (c *Config) ListSSHServers(query string) []*SSHServer {
	q := strings.ToLower(query)
	out := []*SSHServer{}
	for _, s := range c.Servers {
		if matchesQuery(q, s.Name, s.Addr, s.Tags) {
			out = append(out, s)
		}
	}
	return out
}

// ListJumphosts 同 ListSSHServers，作用于 Jumphost。
func (c *Config) ListJumphosts(query string) []*Jumphost {
	q := strings.ToLower(query)
	out := []*Jumphost{}
	for _, j := range c.Jumphosts {
		if matchesQuery(q, j.Name, j.Addr, j.Tags) {
			out = append(out, j)
		}
	}
	return out
}

// ListProxies 同 ListSSHServers，作用于 Proxy。
func (c *Config) ListProxies(query string) []*Proxy {
	q := strings.ToLower(query)
	out := []*Proxy{}
	for _, p := range c.Proxies {
		if matchesQuery(q, p.Name, p.Addr, p.Tags) {
			out = append(out, p)
		}
	}
	return out
}

// matchesQuery 判断 q（已小写化）是否多关键字 AND 命中 name / addr / 任一 tag。
// q 按空白分词（strings.Fields，自动压缩多余空格 / 前后空格），无关键字时返回 true
// （即空 query 或纯空白 query 匹配所有）。
// 每个关键字独立匹配 name / addr / 任一 tag 的子串（大小写不敏感），全部命中才返回 true。
func matchesQuery(q, name, addr string, tags []string) bool {
	keywords := strings.Fields(q)
	if len(keywords) == 0 {
		return true
	}
	name = strings.ToLower(name)
	addr = strings.ToLower(addr)
	for _, kw := range keywords {
		if strings.Contains(name, kw) || strings.Contains(addr, kw) {
			continue
		}
		matched := false
		for _, tag := range tags {
			if strings.Contains(strings.ToLower(tag), kw) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// GetSSHServer 返回指定 name 的 server 指针，不存在返回 error。
func (c *Config) GetSSHServer(name string) (*SSHServer, error) {
	for _, s := range c.Servers {
		if s.Name == name {
			return s, nil
		}
	}
	return nil, fmt.Errorf("ssh server %q not found", name)
}

// GetJumphost 返回指定 name 的 jumphost 指针，不存在返回 error。
func (c *Config) GetJumphost(name string) (*Jumphost, error) {
	for _, j := range c.Jumphosts {
		if j.Name == name {
			return j, nil
		}
	}
	return nil, fmt.Errorf("jumphost %q not found", name)
}

// GetProxy 返回指定 name 的 proxy 指针，不存在返回 error。
func (c *Config) GetProxy(name string) (*Proxy, error) {
	for _, p := range c.Proxies {
		if p.Name == name {
			return p, nil
		}
	}
	return nil, fmt.Errorf("proxy %q not found", name)
}

// UpdateSSHServer 应用 RFC 7396 JSON Merge Patch 到指定 name 的 server。
//   - patch=null：删除 server。name 不存在视为 no-op。
//   - patch=object：合并到现有 server；不存在则创建。name 字段强制为参数值。
//   - 合并后重新解析引用 + 校验；失败回滚到原状态。
func (c *Config) UpdateSSHServer(name string, patch json.RawMessage) error {
	if isNullPatch(patch) {
		orig := c.Servers
		c.Servers = deleteSSHServer(c.Servers, name)
		if len(c.Servers) == len(orig) {
			return nil // not found, no-op
		}
		if err := c.revalidate(); err != nil {
			c.Servers = orig
			return err
		}
		return nil
	}
	idx := -1
	for i, s := range c.Servers {
		if s.Name == name {
			idx = i
			break
		}
	}
	var origJSON []byte
	if idx >= 0 {
		var err error
		origJSON, err = json.Marshal(c.Servers[idx])
		if err != nil {
			return fmt.Errorf("marshal server: %w", err)
		}
	}
	merged, err := mergePatch(origJSON, patch)
	if err != nil {
		return fmt.Errorf("merge patch: %w", err)
	}
	var newServer SSHServer
	if err := json.Unmarshal(merged, &newServer); err != nil {
		return fmt.Errorf("parse merged server: %w", err)
	}
	newServer.Name = name

	var origPtr *SSHServer
	if idx >= 0 {
		origPtr = c.Servers[idx]
		c.Servers[idx] = &newServer
	} else {
		c.Servers = append(c.Servers, &newServer)
	}
	if err := c.revalidate(); err != nil {
		if idx >= 0 {
			c.Servers[idx] = origPtr
		} else {
			c.Servers = c.Servers[:len(c.Servers)-1]
		}
		return err
	}
	return nil
}

// UpdateJumphost 同 UpdateSSHServer，作用于 Jumphost。
func (c *Config) UpdateJumphost(name string, patch json.RawMessage) error {
	if isNullPatch(patch) {
		orig := c.Jumphosts
		c.Jumphosts = deleteJumphost(c.Jumphosts, name)
		if len(c.Jumphosts) == len(orig) {
			return nil
		}
		if err := c.revalidate(); err != nil {
			c.Jumphosts = orig
			return err
		}
		return nil
	}
	idx := -1
	for i, j := range c.Jumphosts {
		if j.Name == name {
			idx = i
			break
		}
	}
	var origJSON []byte
	if idx >= 0 {
		var err error
		origJSON, err = json.Marshal(c.Jumphosts[idx])
		if err != nil {
			return fmt.Errorf("marshal jumphost: %w", err)
		}
	}
	merged, err := mergePatch(origJSON, patch)
	if err != nil {
		return fmt.Errorf("merge patch: %w", err)
	}
	var newJH Jumphost
	if err := json.Unmarshal(merged, &newJH); err != nil {
		return fmt.Errorf("parse merged jumphost: %w", err)
	}
	newJH.Name = name

	var origPtr *Jumphost
	if idx >= 0 {
		origPtr = c.Jumphosts[idx]
		c.Jumphosts[idx] = &newJH
	} else {
		c.Jumphosts = append(c.Jumphosts, &newJH)
	}
	if err := c.revalidate(); err != nil {
		if idx >= 0 {
			c.Jumphosts[idx] = origPtr
		} else {
			c.Jumphosts = c.Jumphosts[:len(c.Jumphosts)-1]
		}
		return err
	}
	return nil
}

// UpdateProxy 同 UpdateSSHServer，作用于 Proxy。
func (c *Config) UpdateProxy(name string, patch json.RawMessage) error {
	if isNullPatch(patch) {
		orig := c.Proxies
		c.Proxies = deleteProxy(c.Proxies, name)
		if len(c.Proxies) == len(orig) {
			return nil
		}
		if err := c.revalidate(); err != nil {
			c.Proxies = orig
			return err
		}
		return nil
	}
	idx := -1
	for i, p := range c.Proxies {
		if p.Name == name {
			idx = i
			break
		}
	}
	var origJSON []byte
	if idx >= 0 {
		var err error
		origJSON, err = json.Marshal(c.Proxies[idx])
		if err != nil {
			return fmt.Errorf("marshal proxy: %w", err)
		}
	}
	merged, err := mergePatch(origJSON, patch)
	if err != nil {
		return fmt.Errorf("merge patch: %w", err)
	}
	var newProxy Proxy
	if err := json.Unmarshal(merged, &newProxy); err != nil {
		return fmt.Errorf("parse merged proxy: %w", err)
	}
	newProxy.Name = name

	var origPtr *Proxy
	if idx >= 0 {
		origPtr = c.Proxies[idx]
		c.Proxies[idx] = &newProxy
	} else {
		c.Proxies = append(c.Proxies, &newProxy)
	}
	if err := c.revalidate(); err != nil {
		if idx >= 0 {
			c.Proxies[idx] = origPtr
		} else {
			c.Proxies = c.Proxies[:len(c.Proxies)-1]
		}
		return err
	}
	return nil
}

// revalidate 重新解析引用 + 跑校验。Update* 操作后必须调用。
// 先把 Via/Proxy 指针同步到 viaName/proxyName（兼容内存直接构造、未走 JSON 反序列化的情况），
// 再清空指针、重新解析、最后校验。任何环节失败都视为 Update 失败。
func (c *Config) revalidate() error {
	for _, j := range c.Jumphosts {
		if j.Via != nil {
			j.viaName = j.Via.Name
		}
		if j.Proxy != nil {
			j.proxyName = j.Proxy.Name
		}
	}
	for _, s := range c.Servers {
		if s.Via != nil {
			s.viaName = s.Via.Name
		}
		if s.Proxy != nil {
			s.proxyName = s.Proxy.Name
		}
	}
	for _, j := range c.Jumphosts {
		j.Via = nil
		j.Proxy = nil
	}
	for _, s := range c.Servers {
		s.Via = nil
		s.Proxy = nil
	}
	if err := c.resolveReferences(); err != nil {
		return err
	}
	return c.Validate()
}

// isNullPatch 判断 patch 是否为顶层 null（删除信号）。
func isNullPatch(patch json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(patch), []byte("null"))
}

func deleteSSHServer(items []*SSHServer, name string) []*SSHServer {
	for i, s := range items {
		if s.Name == name {
			return append(items[:i], items[i+1:]...)
		}
	}
	return items
}

func deleteJumphost(items []*Jumphost, name string) []*Jumphost {
	for i, j := range items {
		if j.Name == name {
			return append(items[:i], items[i+1:]...)
		}
	}
	return items
}

func deleteProxy(items []*Proxy, name string) []*Proxy {
	for i, p := range items {
		if p.Name == name {
			return append(items[:i], items[i+1:]...)
		}
	}
	return items
}

// mergePatch 实现 RFC 7396 JSON Merge Patch。
//   - patch 为顶层 null → 返回 (nil, nil)，调用方据此删除。
//   - patch 为 object → 递归合并到 original；null 值删除 key，object 递归合并，其他替换。
//   - patch 非对象非 null → 报错（本工具不允许用整体替换覆盖一个实体）。
//
// original 可为空（表示实体不存在），此时合并到空对象，效果等同于用 patch 创建。
func mergePatch(original []byte, patch json.RawMessage) ([]byte, error) {
	var patchVal any
	if err := json.Unmarshal(patch, &patchVal); err != nil {
		return nil, fmt.Errorf("parse patch: %w", err)
	}
	if patchVal == nil {
		return nil, nil
	}
	patchObj, ok := patchVal.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("patch must be object or null, got %T", patchVal)
	}
	var origVal any
	if len(original) > 0 {
		if err := json.Unmarshal(original, &origVal); err != nil {
			return nil, fmt.Errorf("parse original: %w", err)
		}
	}
	origObj, ok := origVal.(map[string]any)
	if !ok {
		origObj = map[string]any{}
	}
	merged := mergeMaps(origObj, patchObj)
	return json.Marshal(merged)
}

// mergeMaps 递归合并两个 map。patch 中 null 删除 key，object 递归合并，其他值替换。
func mergeMaps(orig, patch map[string]any) map[string]any {
	result := make(map[string]any, len(orig))
	for k, v := range orig {
		result[k] = v
	}
	for k, v := range patch {
		if v == nil {
			delete(result, k)
			continue
		}
		patchObj, isPatchObj := v.(map[string]any)
		if isPatchObj {
			if origVal, exists := result[k]; exists {
				if origObj, isOrigObj := origVal.(map[string]any); isOrigObj {
					result[k] = mergeMaps(origObj, patchObj)
					continue
				}
			}
			result[k] = mergeMaps(map[string]any{}, patchObj)
			continue
		}
		result[k] = v
	}
	return result
}
