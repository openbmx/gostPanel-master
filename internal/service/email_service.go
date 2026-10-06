package service

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	stderrors "errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"

	"gost-panel/internal/dto"
	"gost-panel/internal/errors"
	"gost-panel/pkg/logger"
)

// 此前没有任何超时，SMTP 服务器握手后不再响应时，测试请求会一直挂着。
// 会话超时要留足余量：服务器收完正文后才做反垃圾检查，可能要十几秒才应答。
// 前端为测试邮件单独设置了 60 秒超时，必须大于这里的会话超时。（变量而非常量，便于测试缩短）
var (
	smtpDialTimeout    = 10 * time.Second
	smtpSessionTimeout = 45 * time.Second
)

// SendTestEmail 发送测试邮件
func (s *SystemConfigService) SendTestEmail(req *dto.EmailConfigReq) error {
	// 验证必要参数
	if req.Host == "" || req.Port == 0 || req.FromEmail == "" {
		return errors.ErrSMTPConfigIncomplete
	}

	toEmail := req.FromEmail
	if req.ToEmail != "" {
		toEmail = req.ToEmail
	}

	// 信封与邮件头都用解析后的纯地址：填成 "名字 <a@b.com>" 时，
	// 原样塞进 MAIL FROM / RCPT TO 会被服务器当作语法错误拒绝
	from, err := mail.ParseAddress(req.FromEmail)
	if err != nil {
		return errors.ErrSMTPAddressInvalid
	}
	to, err := mail.ParseAddress(toEmail)
	if err != nil {
		return errors.ErrSMTPAddressInvalid
	}

	siteTitle := "Gost Panel"
	if cfg, err := s.repo.Get(); err == nil && strings.TrimSpace(cfg.SiteTitle) != "" {
		siteTitle = strings.TrimSpace(cfg.SiteTitle)
	}

	msg, err := buildTestEmail(siteTitle, from.Address, to.Address, time.Now())
	if err != nil {
		return errors.ErrSMTPAddressInvalid
	}
	return sendMail(req, from.Address, to.Address, msg)
}

// buildTestEmail 构造一封符合 RFC 5322 / MIME 规范的测试邮件。
//
// 旧实现只写了 To 和 Subject 两个头：没有 From、Date、Message-ID，也没有 MIME 版本与
// 字符集声明，中文标题和正文直接以 8bit 原样发出 —— 这些都是反垃圾引擎的典型扣分项，
// issue #4 中阿里企业邮箱就直接以 ESO_LOCAL_SPAM 退信。
func buildTestEmail(siteTitle, from, to string, now time.Time) ([]byte, error) {
	fromAddr, err := mail.ParseAddress(from)
	if err != nil {
		return nil, fmt.Errorf("发件人地址无效: %w", err)
	}
	toAddr, err := mail.ParseAddress(to)
	if err != nil {
		return nil, fmt.Errorf("收件人地址无效: %w", err)
	}

	messageID, err := newMessageID(addressDomain(fromAddr.Address))
	if err != nil {
		return nil, err
	}

	body := fmt.Sprintf("这是一封来自 %s 的测试邮件。\r\n"+
		"如果您收到这封邮件，说明 SMTP 配置正确，面板可以正常发送邮件。\r\n"+
		"\r\n"+
		"发送时间：%s\r\n", siteTitle, now.Format("2006-01-02 15:04:05 -0700"))

	var b strings.Builder
	// mail.Address 会按 RFC 2047 编码非 ASCII 的显示名
	writeHeader(&b, "From", (&mail.Address{Name: siteTitle, Address: fromAddr.Address}).String())
	writeHeader(&b, "To", (&mail.Address{Address: toAddr.Address}).String())
	writeHeader(&b, "Subject", mime.BEncoding.Encode("UTF-8", siteTitle+" 测试邮件"))
	writeHeader(&b, "Date", now.Format(time.RFC1123Z))
	writeHeader(&b, "Message-ID", messageID)
	writeHeader(&b, "MIME-Version", "1.0")
	writeHeader(&b, "Content-Type", "text/plain; charset=UTF-8")
	writeHeader(&b, "Content-Transfer-Encoding", "base64")
	b.WriteString("\r\n")

	// base64 正文按 RFC 2045 每行不超过 76 个字符
	encoded := base64.StdEncoding.EncodeToString([]byte(body))
	for len(encoded) > 76 {
		b.WriteString(encoded[:76])
		b.WriteString("\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded)
	b.WriteString("\r\n")

	return []byte(b.String()), nil
}

func writeHeader(b *strings.Builder, key, value string) {
	b.WriteString(key)
	b.WriteString(": ")
	b.WriteString(value)
	b.WriteString("\r\n")
}

// newMessageID 生成全局唯一的 Message-ID，形如 <随机串@发件域名>
func newMessageID(domain string) (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	if domain == "" {
		domain = "localhost"
	}
	return fmt.Sprintf("<%s.%s@%s>", strconv.FormatInt(time.Now().UnixNano(), 36), hex.EncodeToString(buf), domain), nil
}

func addressDomain(address string) string {
	if i := strings.LastIndex(address, "@"); i >= 0 {
		return address[i+1:]
	}
	return ""
}

// heloName 选择 EHLO 时报告的主机名。
// net/smtp 默认报告 "localhost"，不少反垃圾引擎会因此扣分；优先用本机的完整域名，
// 不是 FQDN（容器里常见一串随机 ID）时退回发件人的域名。
func heloName(from string) string {
	if host, err := os.Hostname(); err == nil && strings.Contains(host, ".") {
		return host
	}
	if addr, err := mail.ParseAddress(from); err == nil {
		if domain := addressDomain(addr.Address); domain != "" {
			return domain
		}
	}
	return "localhost"
}

// smtpError 把底层错误包装成带服务器原始应答的业务错误，管理员据此才能判断是账号、
// 发信策略还是反垃圾的问题。超时单独报告：服务器只是应答慢时，邮件可能已经投递，
// 不能误报成“拒收”。
func smtpError(base *errors.BizError, err error) error {
	var netErr net.Error
	if stderrors.As(err, &netErr) && netErr.Timeout() {
		return errors.WithDetail(errors.ErrSMTPTimeout, err.Error())
	}
	return errors.WithDetail(base, err.Error())
}

// sendMail 通过 SMTP 投递一封邮件，from/to 为解析后的纯邮箱地址。
// 465 端口按惯例走隐式 TLS（SMTPS）；其他端口先明文连接，服务器支持时升级为 STARTTLS。
func sendMail(req *dto.EmailConfigReq, from, to string, msg []byte) error {
	addr := net.JoinHostPort(req.Host, strconv.Itoa(req.Port))
	dialer := &net.Dialer{Timeout: smtpDialTimeout}

	// 默认校验服务端证书，防止中间人攻击；如确需连接自签名服务器，
	// 应单独提供显式开关，而不是无条件信任任意证书。
	tlsConfig := &tls.Config{ServerName: req.Host}

	var (
		conn net.Conn
		err  error
	)
	if req.Port == 465 {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsConfig)
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		logger.Warnf("连接 SMTP 服务器 %s 失败: %v", addr, err)
		return smtpError(errors.ErrSMTPConnectFailed, err)
	}
	// 整个会话共用一个截止时间，防止服务器中途不再响应把请求挂死
	_ = conn.SetDeadline(time.Now().Add(smtpSessionTimeout))

	client, err := smtp.NewClient(conn, req.Host)
	if err != nil {
		_ = conn.Close()
		logger.Warnf("创建 SMTP 客户端失败: %v", err)
		return smtpError(errors.ErrSMTPClientFailed, err)
	}
	defer func() {
		_ = client.Close()
	}()

	if err = client.Hello(heloName(from)); err != nil {
		logger.Warnf("SMTP EHLO 失败: %v", err)
		return smtpError(errors.ErrSMTPClientFailed, err)
	}

	if req.Port != 465 {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err = client.StartTLS(tlsConfig); err != nil {
				logger.Warnf("SMTP STARTTLS 失败: %v", err)
				return smtpError(errors.ErrSMTPConnectFailed, fmt.Errorf("STARTTLS: %w", err))
			}
		}
	}

	if req.Username != "" && req.Password != "" {
		if ok, _ := client.Extension("AUTH"); !ok {
			logger.Warnf("SMTP 服务器 %s 不支持 AUTH", addr)
			return errors.WithDetail(errors.ErrSMTPAuthFailed, "服务器未提供 AUTH 认证")
		}
		// PlainAuth 只会在 TLS 连接（或本机）上发送凭据，明文连接会在这里被拒绝
		if err = client.Auth(smtp.PlainAuth("", req.Username, req.Password, req.Host)); err != nil {
			logger.Warnf("SMTP 认证失败: %v", err)
			return smtpError(errors.ErrSMTPAuthFailed, err)
		}
	}

	if err = client.Mail(from); err != nil {
		logger.Warnf("SMTP MAIL FROM 被拒绝: %v", err)
		return smtpError(errors.ErrSMTPSenderFailed, err)
	}
	if err = client.Rcpt(to); err != nil {
		logger.Warnf("SMTP RCPT TO 被拒绝: %v", err)
		return smtpError(errors.ErrSMTPRecipientFailed, err)
	}

	w, err := client.Data()
	if err != nil {
		return smtpError(errors.ErrSMTPDataFailed, err)
	}
	if _, err = w.Write(msg); err != nil {
		return smtpError(errors.ErrSMTPWriteFailed, err)
	}
	if err = w.Close(); err != nil {
		// 服务器收完正文才做内容检查，反垃圾拒收（如阿里邮箱的 ESO_LOCAL_SPAM）出现在这里
		logger.Warnf("SMTP 服务器拒收邮件: %v", err)
		return smtpError(errors.ErrSMTPRejected, err)
	}

	// 走到这里服务器已经接收了邮件；有些服务器对 QUIT 直接断开连接，不能因此报发送失败
	_ = client.Quit()
	return nil
}
