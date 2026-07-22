package pty

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"sshmng/internal/config"
	"sshmng/internal/loginflow"
	"sshmng/internal/ssh/conn"
)

// 默认超时：未指定 timeoutMs 时使用。
const defaultCmdTimeout = 30 * time.Second

// ctrlCDrainTimeout 是 Run 超时后发送 Ctrl-C 然后等待新 PS1 的 drain 超时。
// 远程命令收到 SIGINT 后通常会退出并显示新 prompt；如果在这个时间内没看到 PS1，
// 说明远程命令不响应 SIGINT（如 vim / 交互式 REPL），此时放弃 drain、直接返回，
// 下次 Run 可能仍受残留命令影响但至少不会无限阻塞。
const ctrlCDrainTimeout = 3 * time.Second

// setTokenTimeout 是 Run 写入 setup token 命令后等待 setup sentinel 的超时。
// 正常情况下 bash/zsh 立即执行 setup 命令并 emit sentinel；超时说明 shell 异常
// （如 RC 注入失败导致 PS1 没设上），session 不可用，强制 Close。
const setTokenTimeout = 2 * time.Second

// shellDetectTimeout 是 shell 类型探测的超时。
const shellDetectTimeout = 5 * time.Second

// rcInjectTimeout 是 RC 注入后等待首个 PS1 sentinel 的超时。
const rcInjectTimeout = 5 * time.Second

// loginFlowQuietPeriod 是 LoginFlow Read 在 mustContain 为空时的静默期：
// 连续无新数据超过此时长即返回，避免无限等待。
const loginFlowQuietPeriod = 200 * time.Millisecond

// LoginFlowError 携带 LoginFlow 失败时的 trace，供 MCP handler 在 error 响应中
// 一并返回 login_trace 字段供 Agent 诊断（设计文档 §3.x "LoginFlow 失败 error +
// login_trace"）。trace 含每步的 send / expect / output，Agent 据此修配置重试。
//
// Stage 标识失败发生在哪段 flow（"direct" / "jumphost" / "target"），仅用于日志，
// 不影响 Error() 输出——Err 本身已含足够上下文。
type LoginFlowError struct {
	Stage string
	Trace []loginflow.TraceEntry
	Err   error
}

func (e *LoginFlowError) Error() string { return e.Err.Error() }
func (e *LoginFlowError) Unwrap() error { return e.Err }

// PtyConnOptions 是 NewPtyConn 的可选参数；nil 表示无 LoginFlow（直连场景）。
type PtyConnOptions struct {
	LoginFlow       map[string]config.LoginAction // 非空时在 shell detect 后、RC 注入前执行
	LoginEntry      string                        // LoginFlow 起始 Action 名
	MaxSteps        int                           // 0 = 默认 50
	GlobalTimeoutMs int                           // 0 = 默认 60000
}

// PtyConn 包装 *ssh.Session + PTY，实现 Conn 接口。
// 注入 sentinel 后即可通过 Run 执行单条命令并自动识别命令边界 + exit code。
type PtyConn struct {
	session *ssh.Session
	client  *ssh.Client
	stdin   io.WriteCloser
	stdout  io.Reader
	sid     string
	shell   string
	logger  *slog.Logger

	// sftpClient 在 OpenPtyConn 时尝试建立，5s 超时；失败留空，SftpAvailable()=false。
	sftpClient *sftp.Client

	// 单 reader goroutine：所有读取通过 stdoutCh 序列化，避免多个 goroutine 竞争
	// 同一 SSH channel 的 Read。
	stdoutCh chan []byte
	doneCh   chan struct{}
	closed   bool

	// pushback 缓冲：readUntilPatternTimeout 匹配 pattern 后，pattern 之后的
	// trailing data 存此，下次 Read 优先消费。避免 detectShell 吃掉 LoginFlow
	// 阶段 server 自发输出。
	pushback []byte

	// ctrlCDrainTimeout 是 Run 超时后发 Ctrl-C 然后等待命令退出的 drain 超时。
	// 默认 ctrlCDrainTimeout 常量（3s），测试可覆盖以加速。
	ctrlCDrainTimeout time.Duration

	// setTokenTimeout 是 Run 等待 setup sentinel 的超时。默认 setTokenTimeout 常量（2s），
	// 测试可覆盖以加速。
	setTokenTimeout time.Duration

	mu sync.Mutex
}

// NewPtyConn 是直连场景的便捷构造器：OpenPtyConn + 可选单段 LoginFlow + InjectRC。
// 返回 ready-to-use 的 PtyConn。
//
// Pattern B（两段式 LoginFlow）不要用此构造器——改用 OpenPtyConn + 多次 RunLoginFlow
// + InjectRC，以便在 jumphost flow 与 server flow 之间切分 PTY 流。
//
// opts 为 nil 或 opts.LoginFlow 为空时跳过 LoginFlow（纯直连）。
//
// logger 用于 DEBUG 级别的 PTY 交互日志（Run / SendInput / detectShell / injectRC /
// LoginFlow 每步）；nil 退化为 discard handler。
//
// LoginFlow 失败时返回 *LoginFlowError（携带 trace）；其他失败（detectShell / RC
// 注入）返回普通 error。调用方可用 errors.As 提取 trace 返给 Agent 诊断。
func NewPtyConn(client *ssh.Client, sid string, opts *PtyConnOptions, logger *slog.Logger) (*PtyConn, error) {
	p, err := OpenPtyConnWithTimeout(client, sid, logger, 0)
	if err != nil {
		return nil, err
	}
	if opts != nil && len(opts.LoginFlow) > 0 {
		trace, err := p.RunLoginFlow(opts.LoginFlow, opts.LoginEntry, LoginFlowOptions{
			MaxSteps:        opts.MaxSteps,
			GlobalTimeoutMs: opts.GlobalTimeoutMs,
		})
		if err != nil {
			p.Close()
			return nil, &LoginFlowError{Stage: "direct", Trace: trace, Err: err}
		}
	}
	if err := p.DetectShell(); err != nil {
		p.Close()
		return nil, fmt.Errorf("detect shell: %w", err)
	}
	if err := p.InjectRC(); err != nil {
		p.Close()
		return nil, fmt.Errorf("inject rc: %w", err)
	}
	// 直连场景：SFTP 通道是到 target 的，探测启用。
	p.TryEnableSftp()
	return p, nil
}

// OpenPtyConn 在已建立的 SSH 连接上分配 PTY、启动 shell。
// 不执行 detectShell / LoginFlow / RC 注入——调用方负责后续装配：
//   - 直连：DetectShell → InjectRC
//   - SSHServer.LoginFlow：RunLoginFlow → DetectShell → InjectRC
//   - Pattern B：RunLoginFlow(jumphost) → RunLoginFlow(server) → DetectShell → InjectRC
//
// detectShell 必须在所有 LoginFlow 之后调用：堡垒机/2FA 菜单等场景下，
// session.Shell() 启动的是菜单程序而非真实 shell，此时探测命令无法解析；
// 走完 LoginFlow 才进入目标真 shell，detectShell 才有效。
//
// logger 用于 DEBUG 级别的 PTY 交互日志；nil 退化为 discard handler。
//
// 调用者负责在失败时 Close。
func OpenPtyConn(client *ssh.Client, sid string, logger *slog.Logger) (*PtyConn, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("new session: %w", err)
	}
	// ECHO=0：所有 stdin 写入都是 sshmng 主动发起（LoginFlow Send / run_in_session
	// cmd / send_input text），不需要 tty driver 回显。若 ECHO=1，detectShell 发送的
	// 探测命令会先被回显到 stdout，readUntilPattern 在回显字符串里就匹配到
	// __DETECT_END__，ParseShellDetect 看到的是未展开的 `echo __SHELL_DETECT__:$0:...`
	// （$0 仍是字面量），无法解析 shell 类型。RC 注入里有 stty -echo 但那是 RC 之后
	// 才生效，detectShell 在 RC 之前执行，必须在 pty-req 阶段就关闭 ECHO。
	//
	// ECHO=0 是主防护但不一定生效（Pattern B 非 ssh 堡垒机不传播 ECHO=0、shell RC
	// 执行 stty echo 覆盖、非标 server 忽略 terminal modes）。detectShell 的变量化
	// end marker（`__sshmng_dr=<rand>; ... echo __DETECT_END_${__sshmng_dr}__`）是
	// defense-in-depth：即使回显存在，readUntilPattern 也不在回显行命中 end marker。
	modes := ssh.TerminalModes{
		ssh.ECHO:          0,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := session.RequestPty("xterm", 100, 120, modes); err != nil {
		session.Close()
		return nil, fmt.Errorf("request pty: %w", err)
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		session.Close()
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		session.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := session.Shell(); err != nil {
		session.Close()
		return nil, fmt.Errorf("start shell: %w", err)
	}
	p := &PtyConn{
		session:           session,
		client:            client,
		stdin:             stdin,
		stdout:            stdout,
		sid:               sid,
		logger:            logger,
		stdoutCh:          make(chan []byte, 1024),
		doneCh:            make(chan struct{}),
		ctrlCDrainTimeout: ctrlCDrainTimeout,
		setTokenTimeout:   setTokenTimeout,
	}
	go p.readLoop()

	return p, nil
}

// openPtyHardTimeout 是 OpenPtyConnWithTimeout 的默认硬超时。
// SSH 协议无 per-operation 超时：NewSession / RequestPty / Shell 等 channel request
// 等待 server Reply，server 卡死时永远阻塞。该超时是最后兜底——超时即认为 server
// 不可用，Close client 中断所有阻塞调用。
const openPtyHardTimeout = 15 * time.Second

// OpenPtyConnWithTimeout 是 OpenPtyConn 的带硬超时版本。
// 在 timeout 内未完成则 Close client 中断阻塞的 SSH 操作（RequestPty / Shell），
// 返回 "open pty timed out" 错误。
//
// SSH 协议无 per-operation 超时：session.Shell() 等待 server Reply，server 卡死时
// 永远阻塞。timeout 后 Close client 强制中断底层 conn，使阻塞的调用返回 error，
// goroutine 随后退出。
//
// 超时后 client 被关闭，调用方不能再使用；返回的 *PtyConn 为 nil。
//
// timeout=0 时使用 openPtyHardTimeout 默认值。
func OpenPtyConnWithTimeout(client *ssh.Client, sid string, logger *slog.Logger, timeout time.Duration) (*PtyConn, error) {
	if timeout <= 0 {
		timeout = openPtyHardTimeout
	}
	type result struct {
		p   *PtyConn
		err error
	}
	ch := make(chan result, 1)
	go func() {
		p, err := OpenPtyConn(client, sid, logger)
		ch <- result{p, err}
	}()
	select {
	case r := <-ch:
		return r.p, r.err
	case <-time.After(timeout):
		// Close client 中断阻塞的 SSH 操作（RequestPty / Shell）。
		// goroutine 会随后返回 error，ch 有 buffer 不会泄漏。
		client.Close()
		r := <-ch
		if r.p != nil {
			// 罕见竞态：goroutine 在 timeout 触发前刚成功返回。
			// 此时 p 持有已被我们 Close 的 client，再 Close 一次是 no-op。
			r.p.Close()
		}
		return nil, fmt.Errorf("open pty timed out after %s", timeout)
	}
}

// LoginFlowOptions 是 RunLoginFlow 的可选参数；零值使用 loginflow 包默认值。
type LoginFlowOptions struct {
	MaxSteps        int
	GlobalTimeoutMs int
}

// TryEnableSftp 尝试在已建立的 SSH 连接上打开 sftp 通道。
// 成功后 SftpAvailable()=true，Upload/Download 可用；失败留空，SftpAvailable()=false。
// 不影响 PTY / login 流程。
//
// 调用方决定是否启用：
//   - 直连：调用（SFTP 通道是到 target 的，可用即 upload/download 到 target）
//   - Pattern B（via.ssh_j=false）：不调用（SSH client 是到 jumphost 的，
//     SFTP 通道只会到 jumphost，不能用于上传到 target；探测成功反而误导）
func (p *PtyConn) TryEnableSftp() {
	if sc, err := conn.NewSftpClient(p.client); err == nil {
		p.sftpClient = sc
		p.logger.Debug("sftp channel", "sid", p.sid, "available", true)
	} else {
		p.logger.Debug("sftp channel", "sid", p.sid, "available", false, "err", err.Error())
	}
}

// RunLoginFlow 在 PTY 上执行一段 LoginFlow 决策树。
// 多次调用可串联（Pattern B：先 jumphost flow 再 server flow），trailing data 通过
// pushback 在调用间保留——前段流的最后 Read 不会吞掉后段流要等的 prompt。
//
// flow 为空时直接返回（无操作），便于调用方无条件调用。
// 返回 trace 供诊断（如 MCP handler 在失败时返回 login_trace）。
// 内部把 p.logger 透传给 loginflow.Options，让执行器每步 send/read 都能 Debug 输出。
func (p *PtyConn) RunLoginFlow(flow map[string]config.LoginAction, entry string, opts LoginFlowOptions) ([]loginflow.TraceEntry, error) {
	if len(flow) == 0 {
		return nil, nil
	}
	return loginflow.Run(p, flow, entry, loginflow.Options{
		MaxSteps:       opts.MaxSteps,
		GlobalTimeout:  time.Duration(opts.GlobalTimeoutMs) * time.Millisecond,
		DefaultTimeout: 0, // 用 loginflow 包默认值
		Logger:         p.logger,
	})
}

// InjectRC 注入 RC 脚本并等待首个 PS1 sentinel 出现。
// 在所有 LoginFlow（如有）执行完后调用。
func (p *PtyConn) InjectRC() error {
	return p.injectRC()
}

// DetectShell 探测远端 shell 类型并记录到 p.shell，供后续 InjectRC 生成对应 RC。
// 必须在所有 LoginFlow 之后、InjectRC 之前调用：堡垒机/2FA 菜单等场景下，
// session.Shell() 启动的是菜单程序，探测命令无法解析；走完 LoginFlow 进入
// 真实 shell 后探测才有效。
func (p *PtyConn) DetectShell() error {
	shell, err := p.detectShell()
	if err != nil {
		return fmt.Errorf("detect shell: %w", err)
	}
	p.shell = shell
	p.logger.Debug("shell detected", "sid", p.sid, "shell", shell)
	return nil
}

// readLoop 单 goroutine 持续读 stdout，把数据投递到 stdoutCh。
// 所有 readUntilPattern 都从 stdoutCh 消费，避免多 goroutine 竞争 Read。
func (p *PtyConn) readLoop() {
	tmp := make([]byte, 1024)
	for {
		n, err := p.stdout.Read(tmp)
		if n > 0 {
			data := make([]byte, n)
			copy(data, tmp[:n])
			select {
			case p.stdoutCh <- data:
			case <-p.doneCh:
				return
			}
		}
		if err != nil {
			// 把剩余数据投递完，然后关闭 channel
			close(p.stdoutCh)
			return
		}
	}
}

// Shell 返回探测到的 shell 类型（bash/zsh/dash/ash/...）。
func (p *PtyConn) Shell() string { return p.shell }

// detectShell 发送探测命令并解析 shell 类型。
//
// end marker 用 shell 变量动态构造（`__sshmng_dr=<rand>; ... echo __DETECT_END_${__sshmng_dr}__`），
// 而非字面量 `echo __DETECT_END_<rand>__`。原因：ECHO=0 是主防护（pty-req 阶段关闭
// 回显），但不一定生效（Pattern B 非 ssh 堡垒机不传播 ECHO=0、shell RC 执行
// stty echo 覆盖、非标 server 忽略 terminal modes）。若回显存在且 end marker 是
// 字面量，readUntilPattern 做子串匹配会在回显行命中（回显里含 `echo __DETECT_END_<rand>__`），
// 返回的 output 只有回显行，ParseShellDetect 看不到真正的 echo 命令输出，探测失败。
//
// 变量化后：回显行含 `__DETECT_END_${__sshmng_dr}__`（未展开），readUntilPattern 找的是
// `__DETECT_END_<rand>__`（展开后），子串不匹配，跳过回显行，只在 echo 命令的真正
// 输出处命中。这是 defense-in-depth，不依赖 ECHO=0。
func (p *PtyConn) detectShell() (string, error) {
	rand, err := conn.RandomSID()
	if err != nil {
		return "", err
	}
	cmd := fmt.Sprintf("__sshmng_dr=%s; echo __SHELL_DETECT__:$0:${BASH_VERSION:-}:${ZSH_VERSION:-}; echo __DETECT_END_${__sshmng_dr}__\n", rand)
	p.logger.Debug("detect shell send", "sid", p.sid, "rand", rand, "cmd", cmd)
	if _, err := p.stdin.Write([]byte(cmd)); err != nil {
		return "", fmt.Errorf("write detect cmd: %w", err)
	}
	endMarker := fmt.Sprintf("__DETECT_END_%s__", rand)
	output, err := p.readUntilPattern(endMarker, shellDetectTimeout)
	if err != nil {
		return "", err
	}
	p.logger.Debug("detect shell read", "sid", p.sid, "output", output)
	shell, ok := ParseShellDetect(output, rand)
	if !ok {
		return "", fmt.Errorf("could not parse shell from: %q", output)
	}
	return shell, nil
}

// injectRC 发送 RC 脚本并等待首个 PS1 sentinel 出现。
//
// bash/zsh：等初始 PS1 sentinel `_<rc>__<sid>___]# `（token 为空，rc 是 RC 最后一行
// `export PS1=...` 的退出码 0）。用 regex 匹配（因 sentinel 含动态 exit code）。
//
// dash/ash：等字面量 `__P_<sid>__> `（无 $(echo _$?) 扩展，无 exit code）。
func (p *PtyConn) injectRC() error {
	rc := BuildRC(p.shell, p.sid)
	p.logger.Debug("inject rc send", "sid", p.sid, "shell", p.shell, "rc", rc)
	if _, err := p.stdin.Write([]byte(rc)); err != nil {
		return fmt.Errorf("write rc: %w", err)
	}
	if p.shell == "bash" || p.shell == "zsh" {
		re := initialPS1Re(p.sid)
		_, timedOut := p.readUntilRegexTimeout(re, rcInjectTimeout)
		if timedOut {
			return fmt.Errorf("timeout waiting for initial PS1 sentinel after %s", rcInjectTimeout)
		}
	} else {
		ps1Sentinel := fmt.Sprintf("__P_%s__> ", p.sid)
		if _, err := p.readUntilPattern(ps1Sentinel, rcInjectTimeout); err != nil {
			return err
		}
	}
	p.logger.Debug("inject rc done", "sid", p.sid, "shell", p.shell)
	return nil
}

// initialPS1Re 返回匹配初始 PS1 sentinel 的 regex（bash/zsh）。
// 格式：`_<rc>__<sid>_<token?>__]# `，token 可空（\w* 允许空或任意 word 字符）。
// rc 是 exit code（-?\d+ 允许防御性负数）。
func initialPS1Re(sid string) *regexp.Regexp {
	return regexp.MustCompile(`_-?\d+__` + regexp.QuoteMeta(sid) + `_\w*__]# `)
}

// Run 实现 Conn 接口。发送命令、读取输出、解析 exit code、清洗、截断。
//
// bash/zsh 走 token 化流程（runWithToken）：每次 Run 生成唯一 token，sentinel 含
// token。命令输出不可能预知 token，从根本上杜绝命令/结果错配。
//
// dash/ash 走 PS1-only 流程（runPS1Only）：BuildRC 只覆盖 PS1（`__P_<sid>__> `，
// 不用 `$(echo _$?)`，因 dash/ash 不展开 PS1 中的 `$(...)`），无 exit sentinel，
// exit code 恒为 -1。仅匹配 PS1（无 token），可能误匹配 PS1 字面量，但 dash/ash
// 少见，接受此限制。
//
// 超时处理：超时后发 Ctrl-C + drain 等 sentinel。drain 超时说明远端不响应 SIGINT
// （vim / REPL / 管道阻塞），强制 Close 终止 SSH channel — session 不再可用，
// 但避免远端命令继续跑污染下次 Run。用户需重新 login。
func (p *PtyConn) Run(cmd string, timeoutMs int, maxOutputBytes int) (string, string, int, bool, bool, bool, int, bool, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return "", "", 0, false, false, false, 0, true, errors.New("connection closed")
	}
	p.mu.Unlock()

	timeout := defaultCmdTimeout
	if timeoutMs > 0 {
		timeout = time.Duration(timeoutMs) * time.Millisecond
	}

	if p.shell == "bash" || p.shell == "zsh" {
		return p.runWithToken(cmd, timeout, maxOutputBytes)
	}
	return p.runPS1Only(cmd, timeout, maxOutputBytes)
}

// runWithToken 实现 bash/zsh 的 token 化 Run 流程（6 步）：
//  1. 生成 token = RandomSID()（8 字节 hex）
//  2. 写 setup 命令：`PS1='$(echo _$?)__<sid>_<token>__]# '\n`（token 直接 bake 进 PS1）
//  3. 等精确 <token> 的 sentinel（消费 pushback + stdoutCh 里的旧残留 + setup sentinel）
//  4. 显式清空 pushback（丢弃 setup sentinel 后的任何残留）
//  5. 写 <cmd>\n
//  6. 等精确 <token> 的 sentinel（cmd 的 sentinel）
//
// 关键不变量：
//   - 步骤 3 用精确 token 匹配，旧 token 的 sentinel 字面量（命令 echo 出来的）不会误匹配。
//   - 步骤 4 清空 pushback，确保步骤 6 从干净状态开始。
//   - token 每次 Run 随机，两次 Run 的 token 不同，旧 sentinel 残留不会误匹配新 Run。
//   - 命令输出不可能预知 token（token 是 Run 时随机生成并写入 setup 命令的），
//     路径 A（命令输出含 sentinel 字面量）彻底封死。
func (p *PtyConn) runWithToken(cmd string, timeout time.Duration, maxOutputBytes int) (string, string, int, bool, bool, bool, int, bool, error) {
	token, err := conn.RandomSID()
	if err != nil {
		return "", "", 0, false, false, false, 0, false, fmt.Errorf("generate token: %w", err)
	}

	// 步骤 2：写 setup 命令，升级 PS1 为带 token 的版本。
	// BuildRC 末尾的 `export PS1='$(echo _$?)__<sid>___]# '`（token 空）是 injectRC 等
	// 的初始 PS1；这里动态升级为 `$(echo _$?)__<sid>_<token>__]# `（带 token）。
	// PS1 中 `$(echo _$?)` 在 prompt 展开时输出 exit code，token 直接编码进 PS1 字符串。
	setupCmd := buildSetupTokenCmd(p.sid, token)
	p.logger.Debug("run setup token", "sid", p.sid, "token", token)
	if _, err := p.stdin.Write([]byte(setupCmd)); err != nil {
		return "", "", 0, false, false, false, 0, false, fmt.Errorf("write setup: %w", err)
	}

	// 步骤 3：等 setup sentinel（精确 token）。消费 pushback 里的旧残留（旧 token 或
	// shell 异步输出，不匹配精确 token）+ stdoutCh 直到 setup sentinel 出现。
	// 超时说明 shell 异常（RC 没注入成功、PS1 没设上等），session 不可用。
	// 不自己 Close——返回 connUnusable=true 让 Session 决策（close 在状态机层）。
	setTimeout := p.setTokenTimeout
	if setTimeout == 0 {
		setTimeout = setTokenTimeout
	}
	setupRaw, setupTimedOut := p.readUntilCommandDoneToken(token, setTimeout)
	if setupTimedOut {
		p.logger.Warn("setup token timed out, conn unusable",
			"sid", p.sid, "token", token, "setup_raw", setupRaw)
		return "", "", 0, false, false, false, 0, true, errors.New("setup token timeout")
	}

	// 步骤 4：显式清空 pushback。setup sentinel 后可能有 shell 异步输出残留
	// （如后台任务输出、延迟 prompt），丢弃确保步骤 6 从干净状态开始。
	// 非显而易见：不清空的话，步骤 6 的 readUntilCommandDoneToken 会先消费 pushback，
	// 若残留恰好含当前 token 的 sentinel（如 setup sentinel 的 trailing），会误匹配
	// 导致步骤 6 立刻返回，命令实际未执行。
	p.mu.Lock()
	p.pushback = nil
	p.mu.Unlock()

	// 步骤 5：写命令
	p.logger.Debug("run cmd", "sid", p.sid, "cmd", cmd, "token", token,
		"timeout_ms", timeout.Milliseconds(), "max_output_bytes", maxOutputBytes)
	if _, err := p.stdin.Write([]byte(cmd + "\n")); err != nil {
		return "", "", 0, false, false, false, 0, false, fmt.Errorf("write cmd: %w", err)
	}

	// 步骤 6：等 cmd 的 sentinel（精确 token）
	raw, timedOut := p.readUntilCommandDoneToken(token, timeout)
	ctrlCSent := false
	connUnusable := false
	if timedOut {
		p.logger.Warn("run timed out, sending Ctrl-C",
			"sid", p.sid, "cmd", cmd, "token", token, "timeout", timeout.String())
		if _, err := p.stdin.Write([]byte{0x03}); err != nil {
			p.logger.Error("failed to send Ctrl-C after timeout", "sid", p.sid, "err", err)
		} else {
			ctrlCSent = true
		}
		drainTimeout := p.ctrlCDrainTimeout
		if drainTimeout == 0 {
			drainTimeout = ctrlCDrainTimeout
		}
		drainRaw, drainTimedOut := p.readUntilCommandDoneToken(token, drainTimeout)
		raw = raw + drainRaw
		if drainTimedOut {
			// 远端不响应 SIGINT（vim / REPL / 管道阻塞）。不自己 Close——返回
			// connUnusable=true 让 Session 调 s.Close()（close 决策在状态机层），
			// s.Close() 会调 conn.Close() 终止 SSH channel 杀远端进程。
			p.logger.Warn("Ctrl-C drain timed out, conn unusable",
				"sid", p.sid, "token", token, "drain_timeout", drainTimeout.String())
			connUnusable = true
		}
	}

	code, found := ExtractExitCode(raw, p.sid, token)
	if !found {
		code = -1
	}
	cleaned := CleanOutput(raw, p.sid)
	out, wasTruncated, totalBytes := TruncateOutput(cleaned, maxOutputBytes)
	rawOut, _, _ := TruncateOutput(raw, maxOutputBytes)
	p.logger.Debug("run done",
		"sid", p.sid,
		"token", token,
		"exit_code", code,
		"timed_out", timedOut,
		"ctrl_c_sent", ctrlCSent,
		"raw_bytes", len(raw),
		"cleaned_bytes", len(cleaned),
		"truncated", wasTruncated,
		"total_bytes", totalBytes,
		"output", out,
	)
	return out, rawOut, code, timedOut, ctrlCSent, wasTruncated, totalBytes, connUnusable, nil
}

// buildSetupTokenCmd 构造 setup 命令：升级 PS1 为带 token 的版本。
// bash/zsh 的 PS1 用 `$(echo _$?)` 在 prompt 展开时输出 exit code，token 直接
// 编码进 PS1 字符串，使本次 Run 的 sentinel `_<rc>__<sid>_<token>__]# ` 唯一可识别。
// 不再使用 __sshmng_tok 变量（token 直接 bake 进 PS1，更简单）。
func buildSetupTokenCmd(sid string, token string) string {
	return fmt.Sprintf("PS1='$(echo _$?)__%s_%s__]# '\n", sid, token)
}

// runPS1Only 实现 dash/ash 的 Run 流程：无 token、无 exit sentinel、exit code 恒 -1。
// 仅匹配 PS1 sentinel（`__P_<sid>__> `，无 token）。可能误匹配命令输出里的 PS1
// 字面量，但 dash/ash 少见且不展开 PS1 中的 `$(...)`（无法用 `$(echo _$?)` 捕获
// exit code、无法在 prompt 时动态注入 token），接受此限制。
func (p *PtyConn) runPS1Only(cmd string, timeout time.Duration, maxOutputBytes int) (string, string, int, bool, bool, bool, int, bool, error) {
	p.logger.Debug("run cmd (ps1-only)", "sid", p.sid, "shell", p.shell,
		"cmd", cmd, "timeout_ms", timeout.Milliseconds())
	if _, err := p.stdin.Write([]byte(cmd + "\n")); err != nil {
		return "", "", 0, false, false, false, 0, false, fmt.Errorf("write cmd: %w", err)
	}
	ps1Sentinel := fmt.Sprintf("__P_%s__> ", p.sid)
	raw, timedOut := p.readUntilPatternTimeout(ps1Sentinel, timeout)
	ctrlCSent := false
	connUnusable := false
	if timedOut {
		p.logger.Warn("run timed out, sending Ctrl-C",
			"sid", p.sid, "cmd", cmd, "timeout", timeout.String())
		if _, err := p.stdin.Write([]byte{0x03}); err != nil {
			p.logger.Error("failed to send Ctrl-C after timeout", "sid", p.sid, "err", err)
		} else {
			ctrlCSent = true
		}
		drainTimeout := p.ctrlCDrainTimeout
		if drainTimeout == 0 {
			drainTimeout = ctrlCDrainTimeout
		}
		drainRaw, drainTimedOut := p.readUntilPatternTimeout(ps1Sentinel, drainTimeout)
		raw = raw + drainRaw
		if drainTimedOut {
			// 不自己 Close——返回 connUnusable=true 让 Session 决策（close 在状态机层）。
			p.logger.Warn("Ctrl-C drain timed out, conn unusable",
				"sid", p.sid, "drain_timeout", drainTimeout.String())
			connUnusable = true
		}
	}
	// dash/ash 无 exit sentinel，exit code 恒为 -1
	code := -1
	cleaned := CleanOutput(raw, p.sid)
	out, wasTruncated, totalBytes := TruncateOutput(cleaned, maxOutputBytes)
	rawOut, _, _ := TruncateOutput(raw, maxOutputBytes)
	p.logger.Debug("run done (ps1-only)",
		"sid", p.sid,
		"exit_code", code,
		"timed_out", timedOut,
		"ctrl_c_sent", ctrlCSent,
		"raw_bytes", len(raw),
		"cleaned_bytes", len(cleaned),
		"truncated", wasTruncated,
		"total_bytes", totalBytes,
		"output", out,
	)
	return out, rawOut, code, timedOut, ctrlCSent, wasTruncated, totalBytes, connUnusable, nil
}

// commandDoneReToken 返回匹配精确 token 的 sentinel 正则。
// 格式：`_<rc>__<sid>_<token>__]# `（单 sentinel，含 exit code 和 token）。
// 精确匹配 token：命令输出含旧 token 的 sentinel 字面量不会误匹配当前 Run。
// rc 是命令退出码（-?\d+ 允许防御性负数）。
func commandDoneReToken(sid string, token string) *regexp.Regexp {
	return regexp.MustCompile(`_-?\d+__` + regexp.QuoteMeta(sid) + `_` + regexp.QuoteMeta(token) + `__]# `)
}

// readUntilRegexTimeout 从 stdoutCh 读取直到 regex 匹配或超时。
// 返回 (累积输出到 match 末尾, 是否超时)。match 之后的 trailing 存入 pushback。
func (p *PtyConn) readUntilRegexTimeout(re *regexp.Regexp, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	var buf []byte

	// 先消费 pushback
	p.mu.Lock()
	if len(p.pushback) > 0 {
		buf = append(buf, p.pushback...)
		p.pushback = nil
	}
	p.mu.Unlock()

	if loc := re.FindStringIndex(string(buf)); loc != nil {
		end := loc[1]
		p.mu.Lock()
		p.pushback = append([]byte{}, buf[end:]...)
		p.mu.Unlock()
		return string(buf[:end]), false
	}

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return string(buf), true
		}
		select {
		case data, ok := <-p.stdoutCh:
			if !ok {
				return string(buf), true
			}
			buf = append(buf, data...)
			if loc := re.FindStringIndex(string(buf)); loc != nil {
				end := loc[1]
				p.mu.Lock()
				p.pushback = append([]byte{}, buf[end:]...)
				p.mu.Unlock()
				return string(buf[:end]), false
			}
		case <-time.After(remaining):
			return string(buf), true
		case <-p.doneCh:
			return string(buf), true
		}
	}
}

// readUntilCommandDoneToken 从 stdoutCh 读取直到精确 token 的 sentinel 出现或超时。
// 返回 (累积输出到 sentinel 末尾, 是否超时)。sentinel 之后的 trailing 存入 pushback。
//
// 精确匹配 token：旧 token 的 sentinel 字面量（命令 echo 出来的）不会误匹配，
// 从根本上杜绝命令/结果错配。
func (p *PtyConn) readUntilCommandDoneToken(token string, timeout time.Duration) (string, bool) {
	re := commandDoneReToken(p.sid, token)
	return p.readUntilRegexTimeout(re, timeout)
}

// Read 实现 loginflow.PTY 接口。
//
// matchers 非空时持续累积 PTY 输出，每收到新数据就用所有 matchers（针对 ANSI
// 过滤后的输出）试匹配；命中即返回截至 match 末尾的 raw output，trailing 留 pushback
// 供下次 Read。这是 Pattern B 两段式 LoginFlow 的关键：第一段流的最后一次 Read 不能
// 把 target shell 的 prompt（如 "login: "）一并吞掉——否则第二段流入口 Action 等不到
// 任何输出。
//
// matchers 为空时按静默期 heuristic（连续 loginFlowQuietPeriod 无新数据即返回），
// 用于无 Expects 的 Action 等远端自发输出（MOTD / 菜单）。
//
// 优先消费 pushback（前次 Read 留下的 trailing data）。
func (p *PtyConn) Read(deadline time.Time, matchers []*regexp.Regexp) (string, int, bool, error) {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return "", -1, false, errors.New("connection closed")
	}

	// 先消费 pushback
	p.mu.Lock()
	var buf []byte
	if len(p.pushback) > 0 {
		buf = append(buf, p.pushback...)
		p.pushback = nil
	}
	p.mu.Unlock()

	// pushback 已含匹配：直接返回
	if idx, rawEnd := matchRaw(buf, matchers); idx >= 0 {
		p.mu.Lock()
		p.pushback = append([]byte{}, buf[rawEnd:]...)
		p.mu.Unlock()
		return string(buf[:rawEnd]), idx, false, nil
	}

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return string(buf), -1, true, nil
		}
		timeout := remaining
		// matchers 为空时按静默期返回；否则等到 deadline
		if len(matchers) == 0 && timeout > loginFlowQuietPeriod {
			timeout = loginFlowQuietPeriod
		}
		select {
		case data, ok := <-p.stdoutCh:
			if !ok {
				return string(buf), -1, true, nil
			}
			buf = append(buf, data...)
			if idx, rawEnd := matchRaw(buf, matchers); idx >= 0 {
				p.mu.Lock()
				p.pushback = append([]byte{}, buf[rawEnd:]...)
				p.mu.Unlock()
				return string(buf[:rawEnd]), idx, false, nil
			}
			// matchers 为空时继续 select 等下一个 quiet period
		case <-time.After(timeout):
			if len(matchers) == 0 {
				return string(buf), -1, false, nil
			}
			return string(buf), -1, true, nil
		case <-p.doneCh:
			return string(buf), -1, true, nil
		}
	}
}

// matchRaw 在 raw 输出（含 ANSI）中尝试每个 matcher（针对 stripped 输出）。
// 返回 (matchedIdx, rawEndPos)；matchedIdx=-1 表示未命中。
// rawEndPos 是 match 末尾在 raw 中的字节位置，用于切分 pushback。
func matchRaw(buf []byte, matchers []*regexp.Regexp) (int, int) {
	if len(matchers) == 0 {
		return -1, 0
	}
	stripped, posMap := stripANSIWithPos(string(buf))
	for idx, m := range matchers {
		if loc := m.FindStringIndex(stripped); loc != nil {
			return idx, posMap[loc[1]]
		}
	}
	return -1, 0
}

// stripANSIWithPos 剥离 ANSI CSI / OSC 序列并返回 stripped 输出 + 位置映射。
// posMap[i] = stripped 中第 i 字节对应的 raw 字节偏移；posMap[len(stripped)] = len(raw)。
// 用于在 raw 输出中切分 match 边界（pushback trailing 保留 raw 形式）。
// 使用包级 ansiRe（定义在 normalize.go，同时匹配 CSI 与 OSC）。
func stripANSIWithPos(s string) (string, []int) {
	var stripped strings.Builder
	posMap := make([]int, 0, len(s)+1)
	posMap = append(posMap, 0) // posMap[0] = 0: stripped 起点对应 raw 起点偏移 0

	matches := ansiRe.FindAllStringIndex(s, -1)
	lastEnd := 0
	for _, m := range matches {
		// 拷贝 [lastEnd, m[0]) 区间的 raw 字节
		for i := lastEnd; i < m[0]; i++ {
			stripped.WriteByte(s[i])
			posMap = append(posMap, i+1)
		}
		// 跳过 [m[0], m[1]) 区间的 ANSI 序列
		lastEnd = m[1]
	}
	// 拷贝尾部 [lastEnd, len(s)) 区间的 raw 字节
	for i := lastEnd; i < len(s); i++ {
		stripped.WriteByte(s[i])
		posMap = append(posMap, i+1)
	}
	return stripped.String(), posMap
}

// Send 实现 loginflow.PTY 接口。向 PTY stdin 写入任意文本（LoginFlow 阶段用）。
func (p *PtyConn) Send(s string) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return errors.New("connection closed")
	}
	p.mu.Unlock()
	p.logger.Debug("loginflow send", "sid", p.sid, "bytes", len(s), "text", s)
	_, err := p.stdin.Write([]byte(s))
	return err
}

// Closed 实现 Conn 接口。返回 conn 是否已关闭。
// Run 可能在内部自动 Close（drain 超时 force-close），Session 需在 Run 返回后
// 检查此方法以同步状态——否则 session 会"假活"（state=idle 但 conn 已死）。
func (p *PtyConn) Closed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// Close 实现 Conn 接口。关闭 sftp 通道、session 与底层 SSH client。
// 重复调用是 no-op。session / client 为 nil 时跳过（测试构造的 PtyConn 可能没有）。
func (p *PtyConn) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	sftpClient := p.sftpClient
	p.sftpClient = nil
	session := p.session
	client := p.client
	p.mu.Unlock()

	close(p.doneCh)
	var errs []string
	if sftpClient != nil {
		if err := sftpClient.Close(); err != nil {
			errs = append(errs, fmt.Sprintf("sftp: %v", err))
		}
	}
	if session != nil {
		if err := session.Close(); err != nil {
			errs = append(errs, fmt.Sprintf("session: %v", err))
		}
	}
	if client != nil {
		if err := client.Close(); err != nil {
			errs = append(errs, fmt.Sprintf("client: %v", err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("close: %s", strings.Join(errs, "; "))
	}
	return nil
}

// readUntilPattern 从 stdoutCh 读取直到 data 包含 pattern 或超时。
// 超时返回错误。
func (p *PtyConn) readUntilPattern(pattern string, timeout time.Duration) (string, error) {
	out, timedOut := p.readUntilPatternTimeout(pattern, timeout)
	if timedOut {
		return out, fmt.Errorf("timeout waiting for %q after %s; got: %q", pattern, timeout, out)
	}
	return out, nil
}

// readUntilPatternTimeout 从 stdoutCh 读取直到 data 包含 pattern 或超时。
// 返回 (累积输出, 是否超时)。pattern 之后的 trailing data 存入 pushback 供下次 Read。
func (p *PtyConn) readUntilPatternTimeout(pattern string, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	var buf []byte

	// 先消费 pushback
	p.mu.Lock()
	if len(p.pushback) > 0 {
		buf = append(buf, p.pushback...)
		p.pushback = nil
	}
	p.mu.Unlock()

	if idx := bytes.Index(buf, []byte(pattern)); idx >= 0 {
		// pushback 已包含 pattern：切分，trailing 回存
		end := idx + len(pattern)
		p.mu.Lock()
		p.pushback = append([]byte{}, buf[end:]...)
		p.mu.Unlock()
		return string(buf[:end]), false
	}

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return string(buf), true
		}
		select {
		case data, ok := <-p.stdoutCh:
			if !ok {
				// reader 已关闭
				return string(buf), true
			}
			buf = append(buf, data...)
			if idx := bytes.Index(buf, []byte(pattern)); idx >= 0 {
				end := idx + len(pattern)
				p.mu.Lock()
				p.pushback = append([]byte{}, buf[end:]...)
				p.mu.Unlock()
				return string(buf[:end]), false
			}
		case <-time.After(remaining):
			return string(buf), true
		case <-p.doneCh:
			return string(buf), true
		}
	}
}
