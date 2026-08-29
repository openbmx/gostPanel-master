# 反向代理与边缘安全

面板默认监听明文 HTTP，且不信任任何转发头。把它暴露到公网前，请按本文配置前置代理。

**本文最关键的一条：反向代理必须"覆写"而不是"追加" `X-Forwarded-For`。**
网上流传最广的 Nginx 写法恰好是错的，会让面板的登录限流与审计日志失去意义。

---

## 为什么这件事重要

面板依赖客户端 IP 做两件事：

1. **登录限流**：同一来源 5 分钟内 5 次失败即封禁
2. **审计日志**：记录登录、改密、查看节点凭据等操作的来源

如果攻击者能自己决定这个 IP 的值，两者同时失效：每次请求换一个 IP 就能无限次爆破，审计日志里留下的也全是伪造地址。

面板侧的防线是 `server.trusted_proxies`：**留空时完全忽略 `X-Forwarded-For`**，只取 TCP 连接的对端地址。只有在这里显式列出代理地址后，面板才会去解析转发头。

但这只挡住了一半。另一半在代理上：如果代理把客户端发来的 `X-Forwarded-For` 原样追加进去，面板信任代理后就会连带信任那段伪造内容。

---

## 面板侧配置

```yaml
server:
  # 只填写直接连到面板的那个代理的地址，不要填 0.0.0.0/0
  trusted_proxies:
    - "127.0.0.1/32"
    - "::1/128"
```

对应的环境变量（Docker）：

```yaml
environment:
  - GOSTPANEL_SERVER_TRUSTED_PROXIES=172.16.0.0/12
```

配置生效后，可以这样自查：故意带一个伪造头访问登录接口，看操作日志里记录的来源是不是真实地址。

```bash
curl -i -X POST https://panel.example.com/api/v1/auth/login \
  -H 'X-Forwarded-For: 1.2.3.4' \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"wrong"}'
```

随后在面板的**操作日志**页筛选 `登录失败`。若来源 IP 显示为 `1.2.3.4`，说明配置有误——要么面板信任了不该信任的代理，要么代理透传了客户端的伪造值。

---

## Nginx

```nginx
# http 块中定义共享限流区。速率请按实际流量调整，下面是保守起点。
limit_conn_zone $binary_remote_addr zone=gostpanel_conn:10m;
limit_req_zone  $binary_remote_addr zone=gostpanel_auth:10m rate=5r/s;
limit_req_zone  $binary_remote_addr zone=gostpanel_api:10m rate=30r/s;

server {
    listen 443 ssl;
    http2 on;
    server_name panel.example.com;

    ssl_certificate     /etc/ssl/certs/panel.crt;
    ssl_certificate_key /etc/ssl/private/panel.key;
    ssl_protocols       TLSv1.2 TLSv1.3;

    # 限制慢速请求头攻击与超大请求头
    client_header_timeout 10s;
    client_body_timeout   30s;
    client_max_body_size  8m;
    large_client_header_buffers 4 16k;
    limit_conn gostpanel_conn 20;

    # 登录接口单独收紧
    location /api/v1/auth/login {
        limit_req zone=gostpanel_auth burst=5 nodelay;
        proxy_pass http://127.0.0.1:39100;
        include /etc/nginx/gostpanel_headers.conf;
    }

    location / {
        limit_req zone=gostpanel_api burst=60 nodelay;
        proxy_pass http://127.0.0.1:39100;
        include /etc/nginx/gostpanel_headers.conf;
    }
}
```

`/etc/nginx/gostpanel_headers.conf`：

```nginx
proxy_http_version 1.1;
proxy_set_header Host              $host;
proxy_set_header X-Forwarded-Proto $scheme;

# 关键：用 $remote_addr（TCP 对端）覆写，而不是 $proxy_add_x_forwarded_for。
# $proxy_add_x_forwarded_for 会把客户端发来的 X-Forwarded-For 原样追加，
# 攻击者只要自己带上这个头，就能让面板记录并按任意 IP 限流。
proxy_set_header X-Real-IP        $remote_addr;
proxy_set_header X-Forwarded-For  $remote_addr;
```

> 如果 Nginx 本身也在 CDN 之后，`$remote_addr` 会是 CDN 的出口地址。此时需要先用
> `set_real_ip_from` 把可信范围限定为 CDN 的确切出口 CIDR，再使用 `$realip_remote_addr`。
> 切勿在未限定可信范围的情况下使用客户端传入的 `$http_x_forwarded_for`。

---

## Caddy

```caddyfile
{
	servers {
		max_header_size 64KB
		timeouts {
			read_header 10s
			idle 2m
		}
	}
}

panel.example.com {
	tls {
		protocols tls1.2 tls1.3
	}

	reverse_proxy 127.0.0.1:39100 {
		health_uri /api/v1/health
		health_interval 30s
		health_status 200

		# 同样使用 TCP 对端地址覆写，避免透传客户端伪造值
		header_up X-Real-IP       {remote_host}
		header_up X-Forwarded-For {remote_host}
		header_up X-Forwarded-Proto {scheme}
	}
}
```

Caddy 在 CDN 之后时，需要先配置 `trusted_proxies` 为 CDN 的确切出口段，再改用 Caddy 解析出的 `{client_ip}`：

```caddyfile
{
	servers {
		trusted_proxies static 192.0.2.0/24 2001:db8::/32
		client_ip_headers CF-Connecting-IP X-Forwarded-For
	}
}
```

---

## CDN 部署的额外要求

1. **先用防火墙把源站锁死**，只允许 CDN 出口段访问面板端口。否则攻击者绕过 CDN 直连源站，前面所有配置都不起作用。
2. CDN 必须**覆写**客户端 IP 头，而不是追加。
3. 在 CDN/WAF 侧配置连接数、请求体大小、每 IP 速率与机器人挑战——这些在流量到达源站之前就该拦掉。

---

## 能力边界

面板内的限流是**单进程内存**实现，只能削减到达 Go 之后的滥用，无法吸收流量型攻击、TLS 洪水或大规模分布式来源。这些需要上游带宽、CDN/WAF 过滤与云厂商防火墙。

面板重启会清空限流计数——这是刻意的取舍：单管理员面板不值得为此引入 Redis 依赖。持续性的封禁应在边缘完成。

---

## 部署前自查

- [ ] 面板端口未直接暴露公网（仅监听 `127.0.0.1` 或由防火墙限制）
- [ ] 已启用 HTTPS（面板 `server.tls` 或前置代理终结）
- [ ] `server.trusted_proxies` 只列出了直连面板的代理地址，不含 `0.0.0.0/0`
- [ ] 代理使用 `$remote_addr` / `{remote_host}` **覆写** `X-Forwarded-For`
- [ ] 已用上面的 curl 命令验证过审计日志中的来源 IP 无法被伪造
- [ ] 管理员密码已改为强口令（≥10 位、四类字符取三）
- [ ] `jwt.secret` 已设置为固定的强随机值（否则每次重启都要重新登录）
