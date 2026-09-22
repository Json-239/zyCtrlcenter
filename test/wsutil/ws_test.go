// wsutil 模块测试：WebSocket 握手（Sec-WebSocket-Accept）+ 掩码帧收发。
//
// 用手写原始 TCP 客户端验证（不引第三方库），确保与浏览器/标准客户端互通。
package wsutil_test

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/wsutil"
)

func TestUpgradeHandshakeAndEcho(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsutil.Upgrade(w, r)
		if err != nil {
			t.Errorf("升级失败: %v", err)
			return
		}
		defer conn.Close()
		op, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if op != wsutil.OpText {
			t.Errorf("期望文本帧，实际 opcode=%d", op)
		}
		_ = conn.WriteText(append([]byte("echo:"), payload...))
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", u.Host, 2*time.Second)
	if err != nil {
		t.Fatalf("连接测试服务器失败: %v", err)
	}
	defer conn.Close()

	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	req := fmt.Sprintf("GET / HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\n"+
		"Connection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", u.Host, key)
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)

	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(statusLine, "101") {
		t.Fatalf("握手应返回 101，实际 %q", strings.TrimSpace(statusLine))
	}
	accept := ""
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("读取握手响应头失败: %v", err)
		}
		if line == "\r\n" {
			break
		}
		if strings.HasPrefix(strings.ToLower(line), "sec-websocket-accept:") {
			accept = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
		}
	}
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	want := base64.StdEncoding.EncodeToString(sum[:])
	if accept != want {
		t.Fatalf("Sec-WebSocket-Accept 不符\n got: %s\nwant: %s", accept, want)
	}

	// 发送掩码文本帧 "hi"（客户端帧必须掩码）
	payload := []byte("hi")
	mask := []byte{0x11, 0x22, 0x33, 0x44}
	frame := []byte{0x81, 0x80 | byte(len(payload))}
	frame = append(frame, mask...)
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	if _, err := conn.Write(frame); err != nil {
		t.Fatal(err)
	}

	// 读回服务端帧（不加掩码）
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(br, hdr); err != nil {
		t.Fatalf("读取服务端帧失败: %v", err)
	}
	if hdr[0] != 0x81 {
		t.Fatalf("期望文本帧 0x81，实际 %#x", hdr[0])
	}
	if hdr[1]&0x80 != 0 {
		t.Fatal("服务端帧不应带掩码")
	}
	n := int(hdr[1] & 0x7F)
	body := make([]byte, n)
	if _, err := io.ReadFull(br, body); err != nil {
		t.Fatal(err)
	}
	if string(body) != "echo:hi" {
		t.Fatalf("回显内容不符，实际 %q", string(body))
	}
}

func TestUpgradeRejectsNonWebsocket(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := wsutil.Upgrade(w, r); err == nil {
			t.Error("普通 HTTP 请求不应升级成功")
		}
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
}
