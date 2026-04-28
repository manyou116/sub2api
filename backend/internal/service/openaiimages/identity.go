package openaiimages

import (
	"fmt"

	"github.com/google/uuid"
)

// StableUUIDForAccount 派生与 account 绑定的稳定 UUIDv5。用于在没有持久化
// device_id / session_id 时给 oai-device-id / oai-session-id 头一个一致的值，
// 避免被 ChatGPT / Cloudflare 识别为"全新设备"而触发挑战。
//
// kind 用 "device" / "session" 区分，保证两值不同但各自跨请求稳定。
//
// 这是参照 chatgpt2api 的关键反爬措施。所有需要构造 PoolAccountView 并最终
// 走到 webdriver 的入口（正常 /v1/images/* 调用、账号管理面板的"测试连接"等）
// 都必须用同一份回退链，否则会被 CF 403 challenge。
func StableUUIDForAccount(accountID int64, kind string) string {
	ns := uuid.NameSpaceOID
	name := fmt.Sprintf("openaiimages:%s:%d", kind, accountID)
	return uuid.NewSHA1(ns, []byte(name)).String()
}

// ResolveDeviceSession 按统一回退链解析账号的 device_id / session_id：
//  1. 优先用持久化值（来自 account.extra，调用者传入）
//  2. 其次用旧的 credentials 字段（兼容历史数据）
//  3. 最后按 accountID 派生稳定 UUIDv5
func ResolveDeviceSession(accountID int64, persistedDeviceID, persistedSessionID, credDeviceID, credSessionID string) (deviceID, sessionID string) {
	deviceID = persistedDeviceID
	sessionID = persistedSessionID
	if deviceID == "" && credDeviceID != "" {
		deviceID = credDeviceID
	}
	if sessionID == "" && credSessionID != "" {
		sessionID = credSessionID
	}
	if deviceID == "" {
		deviceID = StableUUIDForAccount(accountID, "device")
	}
	if sessionID == "" {
		sessionID = StableUUIDForAccount(accountID, "session")
	}
	return deviceID, sessionID
}
