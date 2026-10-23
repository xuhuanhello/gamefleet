package web

import (
	"fmt"
	"html/template"
	"math"
	"strconv"
	"strings"
	"time"
)

// gauge is an amount out of a total, ready to show.
type gauge struct {
	Known       bool
	Used, Total string
	Percent     int
}

func cpuGauge(used, total int64, known bool) gauge {
	return gauge{Known: known && total > 0, Used: formatCPU(used), Total: formatCPU(total), Percent: percent(used, total)}
}

func memoryGauge(used, total int64, known bool) gauge {
	return gauge{Known: known && total > 0, Used: formatMemory(used), Total: formatMemory(total), Percent: percent(used, total)}
}

func percent(used, total int64) int {
	if total <= 0 {
		return 0
	}
	return int((used*100 + total/2) / total)
}

// formatCPU shows millicores as cores with two decimals at most: "0.09", "1.5", "2".
func formatCPU(milli int64) string {
	return strconv.FormatFloat(math.Round(float64(milli)/10)/100, 'f', -1, 64)
}

func formatMemory(bytes int64) string {
	const mi, gi = 1 << 20, 1 << 30
	switch {
	case bytes >= 10*gi:
		return fmt.Sprintf("%d GiB", (bytes+gi/2)/gi)
	case bytes >= gi:
		return fmt.Sprintf("%.1f GiB", float64(bytes)/gi)
	default:
		return fmt.Sprintf("%d MiB", (bytes+mi/2)/mi)
	}
}

// age is how long ago t was, the way kubectl shows it: 45s, 12m, 5h12m, 6d3h.
func (c *Console) age(t time.Time) string {
	if t.IsZero() {
		return "–"
	}
	d := c.now().Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(int(d.Seconds()), 0))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

// imageTag is the tag or short digest of an image, which names its build.
func imageTag(image string) string {
	if _, digest, ok := strings.Cut(image, "@"); ok {
		return digest[:min(len(digest), len("sha256:")+12)]
	}
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		return image[i+1:]
	}
	return "latest"
}

// stateClass picks the badge colour for a node, pod or GameServer state.
func stateClass(state string) string {
	switch state {
	case "Ready", "Running", "Normal", "Succeeded":
		return "ok"
	case "Allocated", "Reserved":
		return "busy"
	case "Scheduled", "Creating", "Starting", "RequestReady", "Pending", "PortAllocation", "ContainerCreating",
		"Shutdown", "Terminating", "Completed":
		return "wait"
	default:
		return "bad"
	}
}

// stateLabel names a GameServer, pod or node state in plain words. States
// without a better word keep the Kubernetes name, which shows on hover too.
func stateLabel(state string) string {
	labels := map[string]string{
		"Allocated":         "对局中",
		"Ready":             "空闲",
		"Reserved":          "已预留",
		"Scheduled":         "启动中",
		"Creating":          "启动中",
		"Starting":          "启动中",
		"RequestReady":      "启动中",
		"PortAllocation":    "启动中",
		"Shutdown":          "关闭中",
		"Unhealthy":         "异常",
		"Error":             "出错",
		"Running":           "运行中",
		"Pending":           "等待调度",
		"ContainerCreating": "创建中",
		"Succeeded":         "已结束",
		"Completed":         "已结束",
		"Failed":            "失败",
		"Terminating":       "正在停止",
	}
	if label, ok := labels[state]; ok {
		return label
	}
	return state
}

// shortDigest keeps "sha256:" and the first 12 hex digits.
func shortDigest(digest string) string {
	return digest[:min(len(digest), len("sha256:")+12)]
}

// shortRevision is the first 8 characters of a commit.
func shortRevision(revision string) string {
	if revision == "" {
		return "–"
	}
	return revision[:min(len(revision), 8)]
}

var deployStates = map[string][2]string{
	"new":      {"plain", "待上线"},
	"serving":  {"ok", "服务中"},
	"draining": {"wait", "排空中"},
	"deleted":  {"plain", "已删除"},
}

// deployState is the badge class and label of a deployment state.
func deployState(state string) [2]string {
	if s, ok := deployStates[state]; ok {
		return s
	}
	return [2]string{"plain", state}
}

// channelLabel names a release channel for people.
func channelLabel(channel string) string {
	switch channel {
	case "stable":
		return "正式"
	case "canary":
		return "灰度"
	}
	return channel
}

var actionLabels = map[string]string{
	"build.register":     "登记构建",
	"deployment.create":  "部署",
	"deployment.delete":  "删除部署",
	"deployment.reclaim": "自动回收",
	"deployment.buffer":  "改空闲数",
	"route.set":          "设置路由",
	"route.promote":      "晋升",
	"route.rollback":     "回滚",
	"route.delete":       "删除路由",
	"key.create":         "创建 key",
	"key.revoke":         "吊销 key",
	"room.shutdown":      "删除战斗服",
	"match.end":          "结束对局",
	"gamefleet.download": "下载新版本",
	"gamefleet.apply":    "更新 GameFleet",
	"node.rename":        "机器改名",
	"cluster.add":        "接入集群",
	"node.retire":        "下线机器",
	"node.keep":          "取消下线",
	"node.remove":        "删除机器",
	"runtime.set":        "设置运行时配置",
	"runtime.delete":     "删除运行时配置",
}

// breakable lets a long identifier such as a compat version wrap after its
// dashes, so it does not push a narrow table wider than its panel.
func breakable(s string) template.HTML {
	return template.HTML(strings.ReplaceAll(template.HTMLEscapeString(s), "-", "-<wbr>"))
}

// actionLabel names an audit log action for people.
func actionLabel(action string) string {
	if label, ok := actionLabels[action]; ok {
		return label
	}
	return action
}

var scopeLabels = map[string]string{
	"push":     "推镜像、登记构建",
	"pull":     "拉镜像",
	"allocate": "给对局分配战斗服",
	"gm":       "查询和结束对局",
}

// scopeLabel says what an API key scope allows.
func scopeLabel(scope string) string {
	if label, ok := scopeLabels[scope]; ok {
		return label
	}
	return scope
}

// until says how long until t, or that it has passed.
func (c *Console) until(t time.Time) string {
	d := t.Sub(c.now())
	if d <= 0 {
		return "到期"
	}
	return c.age(c.now().Add(-d)) + "后"
}
