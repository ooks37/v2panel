package agent

import (
	"encoding/base64"
	"strings"
	"testing"
)

// 这些测试锁的是「真限流」链路的最后一环: 下发给用户的链接里必须带**用户自己的**
// UUID, 而不是 233boy 写进 clients[0] 的那个。
//
// 用错凭据的后果不是"连不上", 而是更隐蔽的"能连但记不到账" —— 所以这里
// 逐协议断言, 而不是只看能不能解析出来。

const (
	origUUID = "11111111-2222-4333-8444-555555555555" // 233boy clients[0]
	userUUID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee" // 面板分配给某个用户的
)

func TestRebindShareURI_VLESSKeepsEverythingButCredential(t *testing.T) {
	orig := "vless://" + origUUID + "@tzq1.example.com:55509" +
		"?encryption=none&security=tls&type=ws&host=tzq1.example.com&path=%2F11111111-2222-4333-8444-555555555555#tzq1"
	su, err := ParseShareURI(orig)
	if err != nil {
		t.Fatalf("解析原始链接失败: %v", err)
	}

	got, err := RebindShareURI(su, userUUID)
	if err != nil {
		t.Fatalf("重绑失败: %v", err)
	}
	// ⚠️ 不能对整个字符串做 substring 断言: 233boy 把 UUID 也用作了 WS path
	// (path=%2F<uuid>), 那个 path 是**节点级**的、所有用户共用, 必须原样保留。
	// 所以只断言"凭据字段"这一处。
	back, err := ParseShareURI(got)
	if err != nil {
		t.Fatalf("重绑后无法再解析: %v", err)
	}
	if back.UUID != userUUID {
		t.Fatalf("凭据没换成用户 UUID, 得到 %q (限流会失效)", back.UUID)
	}
	if back.UUID == origUUID {
		t.Fatalf("凭据仍是 clients[0] 的 UUID (限流会失效)")
	}
	if back.Address != su.Address || back.Port != su.Port {
		t.Errorf("地址/端口被改动: %s:%d -> %s:%d", su.Address, su.Port, back.Address, back.Port)
	}
	if back.Path != su.Path || back.Host != su.Host {
		t.Errorf("path/host 被改动: %q/%q -> %q/%q", su.Path, su.Host, back.Path, back.Host)
	}
	if back.Security != su.Security || back.Network != su.Network {
		t.Errorf("security/network 被改动: %q/%q -> %q/%q", su.Security, su.Network, back.Security, back.Network)
	}
	if back.Tag != su.Tag {
		t.Errorf("tag 被改动: %q -> %q", su.Tag, back.Tag)
	}
	if back.UUID != userUUID {
		t.Errorf("解析回来的凭据不是用户 UUID: %q", back.UUID)
	}
}

func TestRebindShareURI_Trojan(t *testing.T) {
	su, err := ParseShareURI("trojan://" + origUUID + "@t.example.com:55509?security=tls&type=ws&path=%2Fabc#tj")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	got, err := RebindShareURI(su, userUUID)
	if err != nil {
		t.Fatalf("重绑失败: %v", err)
	}
	back, _ := ParseShareURI(got)
	if back.UUID != userUUID {
		t.Fatalf("trojan 凭据没换成用户 UUID, 得到 %q", back.UUID)
	}
	if back.Path != "/abc" {
		t.Errorf("path 丢了: %q", back.Path)
	}
}

func TestRebindShareURI_VMessPreservesUnknownFields(t *testing.T) {
	// 带一个解析器不认识的自定义字段 scy, 以及 aid —— 重绑必须原样保留。
	orig := `{"v":"2","ps":"x","add":"h.example.com","port":"55509","id":"` + origUUID +
		`","aid":"64","net":"ws","type":"none","host":"h.example.com","path":"/p","tls":"tls","scy":"aes-128-gcm"}`
	uri := "vmess://" + b64Std(orig)

	su, err := ParseShareURI(uri)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	got, err := RebindShareURI(su, userUUID)
	if err != nil {
		t.Fatalf("重绑失败: %v", err)
	}
	// vmess 整体是 base64, 不能对密文做 substring 断言 —— 解出来看。
	body := strings.TrimPrefix(got, "vmess://")
	raw, err := decodeB64Loose(body)
	if err != nil {
		t.Fatalf("重绑后的 vmess 无法 base64 解码: %v", err)
	}
	s := string(raw)
	for _, want := range []string{`"scy":"aes-128-gcm"`, `"aid":"64"`, `"net":"ws"`, `"/p"`} {
		if !strings.Contains(s, want) {
			t.Errorf("未知/未处理字段被丢弃, 缺少 %s:\n  %s", want, s)
		}
	}
	if !strings.Contains(s, `"id":"`+userUUID+`"`) {
		t.Errorf("vmess 的 id 不是用户 UUID:\n  %s", s)
	}
	if strings.Contains(s, origUUID) {
		t.Errorf("vmess 里仍有 clients[0] 的 UUID")
	}
}

func TestRebindShareURI_SSKeepsMethod(t *testing.T) {
	orig := "ss://" + b64RawURL("aes-256-gcm:oldpass") + "@s.example.com:8443#ssnode"
	su, err := ParseShareURI(orig)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	got, err := RebindShareURI(su, userUUID)
	if err != nil {
		t.Fatalf("重绑失败: %v", err)
	}
	back, err := ParseShareURI(got)
	if err != nil {
		t.Fatalf("重绑后无法解析: %v", err)
	}
	if back.Method != "aes-256-gcm" {
		t.Errorf("加密方式被改动: %q", back.Method)
	}
	if back.Password != userUUID {
		t.Errorf("ss 密码没换成用户 UUID: %q", back.Password)
	}
	if back.Address != "s.example.com" || back.Port != 8443 {
		t.Errorf("地址/端口被改动: %s:%d", back.Address, back.Port)
	}
	if back.Tag != "ssnode" {
		t.Errorf("tag 丢了: %q", back.Tag)
	}
}

// socks/http 在 conf 里用 accounts[] 而非 clients[], usersync 不会给它们写用户凭据。
// 所以它们**不可重绑** —— 必须被排除在订阅之外, 而不是发一个记不到账的链接。
func TestRebindableScheme(t *testing.T) {
	yes := []string{"vless", "vmess", "trojan", "ss", "VLESS", " Trojan "}
	no := []string{"socks", "http", "dokodemo-door", ""}
	for _, s := range yes {
		if !RebindableScheme(s) {
			t.Errorf("%q 应当可重绑", s)
		}
	}
	for _, s := range no {
		if RebindableScheme(s) {
			t.Errorf("%q 不应当可重绑 (conf 里没有 per-user 凭据)", s)
		}
	}
}

func TestRebindShareURI_RejectsUnsupportedAndEmpty(t *testing.T) {
	su := &ShareURI{Scheme: "socks", URI: "socks://x@y:1"}
	if _, err := RebindShareURI(su, userUUID); err == nil {
		t.Error("socks 应当被拒绝")
	}
	su2, _ := ParseShareURI("vless://" + origUUID + "@a.com:443?type=tcp")
	if _, err := RebindShareURI(su2, "   "); err == nil {
		t.Error("空凭据应当被拒绝")
	}
	if _, err := RebindShareURI(nil, userUUID); err == nil {
		t.Error("nil 应当被拒绝")
	}
}

// —— 测试辅助 ——
//
// b64Std 复用 parser_test.go 里的实现 (同包, 重复声明会编译失败)。
// 这里只补 SIP002 userinfo 用的 RawURL 变体; decodeB64Loose 四种编码都认。
func b64RawURL(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}
