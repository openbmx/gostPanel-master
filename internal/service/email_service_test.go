package service

import (
	"bufio"
	"encoding/base64"
	stderrors "errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/mail"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gost-panel/internal/dto"
	"gost-panel/internal/errors"
	"gost-panel/internal/model"
	"gost-panel/internal/repository"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// TestBuildTestEmail_IsWellFormed 回归 issue #4：旧实现只有 To/Subject 两个头，
// 缺 From/Date/Message-ID/MIME 声明、中文未编码，被阿里企业邮箱以 ESO_LOCAL_SPAM 退信。
func TestBuildTestEmail_IsWellFormed(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 30, 0, 0, time.FixedZone("CST", 8*3600))
	raw, err := buildTestEmail("我的面板", "noreply@example.com", "ops@example.org", now)
	if err != nil {
		t.Fatalf("构造邮件失败: %v", err)
	}

	for i, line := range strings.Split(strings.TrimSuffix(string(raw), "\r\n"), "\r\n") {
		if strings.ContainsAny(line, "\r\n") {
			t.Fatalf("第 %d 行含有裸 CR/LF，SMTP 要求 CRLF 换行: %q", i+1, line)
		}
		if len(line) > 998 {
			t.Fatalf("第 %d 行超过 RFC 5322 的 998 字符上限", i+1)
		}
	}

	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("邮件无法按 RFC 5322 解析: %v", err)
	}
	h := msg.Header

	from, err := mail.ParseAddress(h.Get("From"))
	if err != nil || from.Address != "noreply@example.com" || from.Name != "我的面板" {
		t.Errorf("From 头不对: %q (%v)", h.Get("From"), err)
	}
	if to, err := mail.ParseAddress(h.Get("To")); err != nil || to.Address != "ops@example.org" {
		t.Errorf("To 头不对: %q", h.Get("To"))
	}

	subject, err := new(mime.WordDecoder).DecodeHeader(h.Get("Subject"))
	if err != nil || subject != "我的面板 测试邮件" {
		t.Errorf("Subject 未正确编码: %q -> %q (%v)", h.Get("Subject"), subject, err)
	}
	if strings.ContainsFunc(h.Get("Subject"), func(r rune) bool { return r > 127 }) {
		t.Error("Subject 头里不能直接出现非 ASCII 字符")
	}

	if date, err := h.Date(); err != nil || !date.Equal(now) {
		t.Errorf("Date 头缺失或不对: %q (%v)", h.Get("Date"), err)
	}
	if id := h.Get("Message-ID"); !strings.HasPrefix(id, "<") || !strings.HasSuffix(id, "@example.com>") {
		t.Errorf("Message-ID 不对: %q", id)
	}
	if h.Get("MIME-Version") != "1.0" {
		t.Errorf("缺少 MIME-Version 头")
	}
	if ct := h.Get("Content-Type"); ct != "text/plain; charset=UTF-8" {
		t.Errorf("Content-Type 不对: %q", ct)
	}
	if h.Get("Content-Transfer-Encoding") != "base64" {
		t.Errorf("Content-Transfer-Encoding 应为 base64")
	}

	body, _ := io.ReadAll(msg.Body)
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\r\n") {
		if len(line) > 76 {
			t.Fatalf("base64 正文行超过 76 字符: %d", len(line))
		}
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(strings.TrimSpace(string(body)), "\r\n", ""))
	if err != nil {
		t.Fatalf("正文不是合法的 base64: %v", err)
	}
	if !strings.Contains(string(decoded), "这是一封来自 我的面板 的测试邮件") {
		t.Errorf("正文内容不对: %s", decoded)
	}

	// 两封邮件的 Message-ID 必须不同
	raw2, _ := buildTestEmail("我的面板", "noreply@example.com", "ops@example.org", now)
	msg2, _ := mail.ReadMessage(strings.NewReader(string(raw2)))
	if msg2.Header.Get("Message-ID") == h.Get("Message-ID") {
		t.Error("Message-ID 必须全局唯一")
	}
}

func TestBuildTestEmail_RejectsInvalidAddress(t *testing.T) {
	if _, err := buildTestEmail("p", "not-an-address", "ops@example.org", time.Now()); err == nil {
		t.Error("非法发件人地址应报错")
	}
	if _, err := buildTestEmail("p", "noreply@example.com", "", time.Now()); err == nil {
		t.Error("空收件人地址应报错")
	}
}

// fakeSMTP 一个最小的 SMTP 服务器，记录会话内容；dataReply 为收到正文后的应答
type fakeSMTP struct {
	ln        net.Listener
	dataReply string

	mu   sync.Mutex
	helo string
	from string
	rcpt string
	data string
}

func newFakeSMTP(t *testing.T, dataReply string) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动测试 SMTP 服务器失败: %v", err)
	}
	s := &fakeSMTP{ln: ln, dataReply: dataReply}
	t.Cleanup(func() { _ = ln.Close() })
	go s.serve()
	return s
}

func (s *fakeSMTP) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *fakeSMTP) serve() {
	conn, err := s.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	reply := func(line string) { _, _ = conn.Write([]byte(line + "\r\n")) }

	reply("220 fake.smtp ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		cmd := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(cmd, "EHLO "), strings.HasPrefix(cmd, "HELO "):
			s.mu.Lock()
			s.helo = line[5:]
			s.mu.Unlock()
			reply("250-fake.smtp")
			reply("250 8BITMIME")
		case strings.HasPrefix(cmd, "MAIL FROM:"):
			s.mu.Lock()
			s.from = line[10:]
			s.mu.Unlock()
			reply("250 OK")
		case strings.HasPrefix(cmd, "RCPT TO:"):
			s.mu.Lock()
			s.rcpt = line[8:]
			s.mu.Unlock()
			reply("250 OK")
		case cmd == "DATA":
			reply("354 End data with <CR><LF>.<CR><LF>")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			s.mu.Lock()
			s.data = b.String()
			s.mu.Unlock()
			if s.dataReply == "" {
				// 模拟服务器收完正文后迟迟不应答（例如反垃圾扫描很慢）
				_, _ = io.Copy(io.Discard, r)
				return
			}
			reply(s.dataReply)
		case cmd == "QUIT":
			reply("221 Bye")
			return
		default:
			reply("502 Command not implemented")
		}
	}
}

func newEmailTestService(t *testing.T) *SystemConfigService {
	t.Helper()
	initObserverServiceTestLogger(t)
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "email.db")), &gorm.Config{Logger: gormlogger.Discard})
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := db.AutoMigrate(&model.SystemConfig{}); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	repo := repository.NewSystemConfigRepository(db)
	cfg, err := repo.Get()
	if err != nil {
		t.Fatalf("读取系统配置失败: %v", err)
	}
	cfg.SiteTitle = "Ops 面板"
	if err := repo.Update(cfg); err != nil {
		t.Fatalf("写入系统配置失败: %v", err)
	}
	return NewSystemConfigService(repo)
}

func TestSendTestEmail_DeliversWellFormedMessage(t *testing.T) {
	svc := newEmailTestService(t)
	server := newFakeSMTP(t, "250 2.0.0 queued")

	err := svc.SendTestEmail(&dto.EmailConfigReq{
		Host:      "127.0.0.1",
		Port:      server.port(),
		FromEmail: "noreply@example.com",
		ToEmail:   "ops@example.org",
	})
	if err != nil {
		t.Fatalf("发送测试邮件失败: %v", err)
	}

	server.mu.Lock()
	defer server.mu.Unlock()
	// net/smtp 默认以 localhost 自报家门，这正是反垃圾引擎的扣分项之一
	if server.helo == "" || strings.EqualFold(server.helo, "localhost") {
		t.Errorf("EHLO 名称不应是 localhost: %q", server.helo)
	}
	// 服务器声明 8BITMIME 时 net/smtp 会在 MAIL FROM 后附加 BODY=8BITMIME，只比较地址部分
	if !strings.HasPrefix(server.from, "<noreply@example.com>") || server.rcpt != "<ops@example.org>" {
		t.Errorf("信封地址不对: from=%q rcpt=%q", server.from, server.rcpt)
	}
	msg, err := mail.ReadMessage(strings.NewReader(server.data))
	if err != nil {
		t.Fatalf("服务器收到的邮件无法解析: %v", err)
	}
	if from, _ := mail.ParseAddress(msg.Header.Get("From")); from == nil || from.Name != "Ops 面板" {
		t.Errorf("发件人显示名应取站点标题: %q", msg.Header.Get("From"))
	}
	for _, key := range []string{"Date", "Message-ID", "MIME-Version", "Content-Type"} {
		if msg.Header.Get(key) == "" {
			t.Errorf("服务器收到的邮件缺少 %s 头", key)
		}
	}
}

// TestSendTestEmail_ReportsServerRejection 服务器收完正文后拒收（反垃圾拦截）时，
// 要把服务器的原始应答带给管理员，而不是笼统的“关闭邮件数据流失败”。
func TestSendTestEmail_ReportsServerRejection(t *testing.T) {
	svc := newEmailTestService(t)
	server := newFakeSMTP(t, "554 5.7.1 (13)ESO_LOCAL_SPAM: spamed by local spam engine")

	err := svc.SendTestEmail(&dto.EmailConfigReq{
		Host:      "127.0.0.1",
		Port:      server.port(),
		FromEmail: "noreply@example.com",
		ToEmail:   "ops@example.org",
	})
	var biz *errors.BizError
	if !stderrors.As(err, &biz) {
		t.Fatalf("应返回业务错误，实际 %v", err)
	}
	if biz.Code != errors.ErrSMTPRejected.Code || !strings.Contains(biz.Message, "ESO_LOCAL_SPAM") {
		t.Errorf("拒收错误应带上服务器应答，实际 code=%d msg=%q", biz.Code, biz.Message)
	}
}

// TestSendTestEmail_UsesBareAddressInEnvelope 发件人填成“名字 <地址>”时，
// 信封里只能出现纯地址，否则服务器会以语法错误拒绝 MAIL FROM。
func TestSendTestEmail_UsesBareAddressInEnvelope(t *testing.T) {
	svc := newEmailTestService(t)
	server := newFakeSMTP(t, "250 2.0.0 queued")

	err := svc.SendTestEmail(&dto.EmailConfigReq{
		Host:      "127.0.0.1",
		Port:      server.port(),
		FromEmail: "Ops Team <noreply@example.com>",
		ToEmail:   "值班 <ops@example.org>",
	})
	if err != nil {
		t.Fatalf("发送测试邮件失败: %v", err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if !strings.HasPrefix(server.from, "<noreply@example.com>") || server.rcpt != "<ops@example.org>" {
		t.Errorf("信封里应是纯地址: from=%q rcpt=%q", server.from, server.rcpt)
	}
}

// TestSendTestEmail_ReportsTimeoutDistinctly 服务器收完正文后迟迟不应答时报“超时”而不是“拒收”：
// 超时的邮件可能其实已经投递了。
func TestSendTestEmail_ReportsTimeoutDistinctly(t *testing.T) {
	svc := newEmailTestService(t)
	server := newFakeSMTP(t, "")
	prev := smtpSessionTimeout
	smtpSessionTimeout = 500 * time.Millisecond
	t.Cleanup(func() { smtpSessionTimeout = prev })

	err := svc.SendTestEmail(&dto.EmailConfigReq{
		Host:      "127.0.0.1",
		Port:      server.port(),
		FromEmail: "noreply@example.com",
		ToEmail:   "ops@example.org",
	})
	var biz *errors.BizError
	if !stderrors.As(err, &biz) || biz.Code != errors.ErrSMTPTimeout.Code {
		t.Fatalf("应返回超时错误，实际 %v", err)
	}
}

// TestSMTPAuthFailedDoesNotLogOut 回归：SMTP 认证失败曾返回 HTTP 401，
// 前端拦截器把 401 当作面板登录失效，测试邮件密码填错就把管理员踢回登录页。
func TestSMTPAuthFailedDoesNotLogOut(t *testing.T) {
	if errors.ErrSMTPAuthFailed.HTTPCode == http.StatusUnauthorized {
		t.Fatal("SMTP 认证失败不能返回 401")
	}
}
