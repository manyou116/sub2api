package webdriver

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/sha3"
)

// PoW 算法移植自 chatgpt2api utils/pow.py。
//
// 工作流程：
//   1. buildPowConfig 组装 18 字段的浏览器指纹数组
//   2. solvePow 在 sha3-512 哈希前缀小于难度阈值时返回编码后字符串
//   3. buildRequirementsToken / buildProofToken 在该字符串前加魔术前缀

var powCores = []int{8, 16, 24, 32}

var powDocumentKeys = []string{
	"_reactListeningo743lnnpvdg",
	"location",
}

var powNavigatorKeys = []string{
	"registerProtocolHandler\u2212function registerProtocolHandler() { [native code] }",
	"storage\u2212[object StorageManager]",
	"locks\u2212[object LockManager]",
	"appCodeName\u2212Mozilla",
	"permissions\u2212[object Permissions]",
	"share\u2212function share() { [native code] }",
	"webdriver\u2212false",
	"managed\u2212[object NavigatorManagedData]",
	"canShare\u2212function canShare() { [native code] }",
	"vendor\u2212Google Inc.",
	"mediaDevices\u2212[object MediaDevices]",
	"vibrate\u2212function vibrate() { [native code] }",
	"storageBuckets\u2212[object StorageBucketManager]",
	"mediaCapabilities\u2212[object MediaCapabilities]",
	"cookieEnabled\u2212true",
	"virtualKeyboard\u2212[object VirtualKeyboard]",
	"product\u2212Gecko",
	"presentation\u2212[object Presentation]",
	"onLine\u2212true",
	"mimeTypes\u2212[object MimeTypeArray]",
	"credentials\u2212[object CredentialsContainer]",
	"serviceWorker\u2212[object ServiceWorkerContainer]",
	"keyboard\u2212[object Keyboard]",
	"gpu\u2212[object GPU]",
	"doNotTrack",
	"serial\u2212[object Serial]",
	"pdfViewerEnabled\u2212true",
	"language\u2212zh-CN",
	"geolocation\u2212[object Geolocation]",
	"userAgentData\u2212[object NavigatorUAData]",
	"getUserMedia\u2212function getUserMedia() { [native code] }",
	"sendBeacon\u2212function sendBeacon() { [native code] }",
	"hardwareConcurrency\u221232",
	"windowControlsOverlay\u2212[object WindowControlsOverlay]",
}

var powWindowKeys = []string{
	"0", "window", "self", "document", "name", "location",
	"customElements", "history", "navigation", "innerWidth", "innerHeight",
	"scrollX", "scrollY", "visualViewport", "screenX", "screenY",
	"outerWidth", "outerHeight", "devicePixelRatio", "screen", "chrome",
	"navigator", "onresize", "performance", "crypto", "indexedDB",
	"sessionStorage", "localStorage", "scheduler", "alert", "atob", "btoa",
	"fetch", "matchMedia", "postMessage", "queueMicrotask", "requestAnimationFrame",
	"setInterval", "setTimeout", "caches",
	"__NEXT_DATA__", "__BUILD_MANIFEST", "__NEXT_PRELOADREADY",
}

var powProcessStart = time.Now()

func powFormatTime() string {
	loc := time.FixedZone("EST", -5*60*60)
	return time.Now().In(loc).Format("Mon Jan 02 2006 15:04:05") + " GMT-0500 (Eastern Standard Time)"
}

// buildPowConfig 构建 PoW 计算配置（18 字段）。
//   index 3 / index 9 在 solvePow 内部被替换为循环变量 i 与 i>>1。
func buildPowConfig(ua, scriptSource, dataBuild string) []any {
	if strings.TrimSpace(scriptSource) == "" {
		scriptSource = defaultSentinelSDKURL
	}
	now := time.Now()
	uptime := float64(time.Since(powProcessStart).Milliseconds())
	return []any{
		[]int{3000, 4000, 5000}[rand.Intn(3)],
		powFormatTime(),
		4294705152,
		0,
		ua,
		scriptSource,
		dataBuild,
		"en-US",
		"en-US,es-US,en,es",
		0,
		powNavigatorKeys[rand.Intn(len(powNavigatorKeys))],
		powDocumentKeys[rand.Intn(len(powDocumentKeys))],
		powWindowKeys[rand.Intn(len(powWindowKeys))],
		uptime,
		uuid.NewString(),
		"",
		powCores[rand.Intn(len(powCores))],
		float64(now.UnixMilli()) - uptime,
	}
}

// solvePow 执行 sha3-512 工作量证明。返回 base64 编码的 nonce 数组。
func solvePow(seed, difficulty string, config []any, limit int) (string, bool) {
	diffBytes, err := hex.DecodeString(difficulty)
	if err != nil {
		return "", false
	}
	diffLen := len(diffBytes)

	raw13, _ := json.Marshal(config[:3])
	static1 := make([]byte, len(raw13))
	copy(static1, raw13)
	static1[len(static1)-1] = ','

	raw49, _ := json.Marshal(config[4:9])
	inner49 := raw49[1 : len(raw49)-1]
	static2 := make([]byte, 0, len(inner49)+2)
	static2 = append(static2, ',')
	static2 = append(static2, inner49...)
	static2 = append(static2, ',')

	raw10, _ := json.Marshal(config[10:])
	static3 := make([]byte, 0, len(raw10))
	static3 = append(static3, ',')
	static3 = append(static3, raw10[1:]...)

	seedBytes := []byte(seed)
	for i := 0; i < limit; i++ {
		iStr := strconv.Itoa(i)
		iHalfStr := strconv.Itoa(i >> 1)

		finalJSON := make([]byte, 0, len(static1)+len(iStr)+len(static2)+len(iHalfStr)+len(static3))
		finalJSON = append(finalJSON, static1...)
		finalJSON = append(finalJSON, iStr...)
		finalJSON = append(finalJSON, static2...)
		finalJSON = append(finalJSON, iHalfStr...)
		finalJSON = append(finalJSON, static3...)

		encoded := base64.StdEncoding.EncodeToString(finalJSON)
		sum := sha3.Sum512(append(seedBytes, encoded...))
		if bytes.Compare(sum[:diffLen], diffBytes) <= 0 {
			return encoded, true
		}
	}
	return "", false
}

// buildRequirementsToken 生成 sentinel chat-requirements 请求体里的 p 字段。
func buildRequirementsToken(ua string, scriptSources []string, dataBuild string) string {
	src := defaultSentinelSDKURL
	if len(scriptSources) > 0 {
		src = scriptSources[rand.Intn(len(scriptSources))]
	}
	cfg := buildPowConfig(ua, src, dataBuild)
	seed := strconv.FormatFloat(rand.Float64(), 'f', -1, 64)
	encoded, ok := solvePow(seed, requirementsTokenDifficulty, cfg, 500000)
	if !ok {
		return ""
	}
	return "gAAAAAC" + encoded
}

// buildProofToken 生成 openai-sentinel-proof-token 头的值。
// 若解题失败，返回 error 让上层换号，而不是发送伪造 token。
func buildProofToken(required bool, seed, difficulty, ua string, scriptSources []string, dataBuild string) (string, error) {
	if !required || strings.TrimSpace(seed) == "" || strings.TrimSpace(difficulty) == "" {
		return "", nil
	}
	src := defaultSentinelSDKURL
	if len(scriptSources) > 0 {
		src = scriptSources[rand.Intn(len(scriptSources))]
	}
	cfg := buildPowConfig(ua, src, dataBuild)
	encoded, ok := solvePow(seed, difficulty, cfg, 500000)
	if !ok {
		return "", errors.New("pow solve failed after 500000 iterations")
	}
	return "gAAAAAB" + encoded, nil
}

// 兼容老调用名（避免内部其他文件改动），但不导出。
var _ = bytes.Compare
