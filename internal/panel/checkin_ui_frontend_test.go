package panel

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"
)

// 本文件钉住签到操作的 UI 判据（2026-11-03 用户反馈）。
//
// 背景：国际版（global）账号上游恒 400 code=10001「签到活动未开启或已过期」，
// 排程亦显式跳过；但旧前端照样渲染可点「签到」按钮，且手动点击后无论成败都
// toast「签到完成」——用户看到「已完成」提示、按钮却永远是「签到」，正是
// 「为什么已经签到的图标不会变成已签到」的根因。两个纯函数只在浏览器里生效，
// Go 编译与后端测试对其完全失明，故用 node harness 钉死：
//   1) 企业版/国际版不渲染死入口（中性标记），CN 账号按钮随 checkin_done 翻转；
//   2) 只有 checkin_done=true 才宣称完成，失败必须如实报原因。

func TestAppJSCheckinCell(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; checkin UI test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function checkinCell');
const end = src.indexOf('function renderAccounts');
if (start < 0 || end < 0 || end < start) throw new Error('checkin region not found');
const ctx = { String, Object };
vm.createContext(ctx);
vm.runInContext(
  'function esc(s){return String(s).replace(/&/g,"&amp;").replace(/</g,"&lt;").replace(/>/g,"&gt;").replace(/"/g,"&quot;");}' +
  src.slice(start, end) +
  '\nthis.checkinCell = checkinCell; this.checkinToastResult = checkinToastResult;', ctx);

const cnTodo = ctx.checkinCell({ uid: 'u1', realm: 'cn' });
const cnDone = ctx.checkinCell({ uid: 'u1', realm: 'cn', checkin_done: true });
const globalCell = ctx.checkinCell({ uid: 'u2', realm: 'global', checkin_done: true });
const entCell = ctx.checkinCell({ uid: 'u3', enterprise: true });

process.stdout.write(JSON.stringify({
  cnTodoHasButton: cnTodo.indexOf('data-a="checkin"') >= 0,
  cnTodoText: cnTodo.indexOf('>签到<') >= 0,
  cnTodoNoDoneTitle: cnTodo.indexOf('今日已签到') < 0,
  cnDoneText: cnDone.indexOf('>已签<') >= 0,
  cnDoneHasTitle: cnDone.indexOf('今日已签到') >= 0,

  // 判别力：国际版即使 checkin_done=true（历史脏状态）也不得渲染可点按钮。
  globalHasNoButton: globalCell.indexOf('data-a="checkin"') < 0,
  globalShowsNeutral: globalCell.indexOf('无签到') >= 0,
  entEmpty: entCell === '',

  ok: ctx.checkinToastResult({ checkin_done: true, credits: 252, credits_total: 550 }),
  okWithMsg: ctx.checkinToastResult({ checkin_done: true, checkin_message: '今天已签到' }),
  fail: ctx.checkinToastResult({ checkin_done: false, credits: 252,
    checkin_message: 'upstream client (http 400): 签到活动未开启或已过期' }),
  failNoMsg: ctx.checkinToastResult({ checkin_done: false }),
}));
`
	scriptFile, err := os.CreateTemp("", "checkin_ui_*.js")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(scriptFile.Name())
	if _, err := scriptFile.WriteString(script); err != nil {
		t.Fatal(err)
	}
	scriptFile.Close()
	raw, err := exec.Command(node, scriptFile.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("node harness failed: %v\n%s", err, raw)
	}
	var got struct {
		CNTodoHasButton   bool `json:"cnTodoHasButton"`
		CNTodoText        bool `json:"cnTodoText"`
		CNTodoNoDoneTitle bool `json:"cnTodoNoDoneTitle"`
		CNDoneText        bool `json:"cnDoneText"`
		CNDoneHasTitle    bool `json:"cnDoneHasTitle"`
		GlobalHasNoButton bool `json:"globalHasNoButton"`
		GlobalShowsNeutral bool `json:"globalShowsNeutral"`
		EntEmpty          bool `json:"entEmpty"`
		OK                struct {
			Text string `json:"text"`
			Kind string `json:"kind"`
		} `json:"ok"`
		OKWithMsg struct {
			Text string `json:"text"`
			Kind string `json:"kind"`
		} `json:"okWithMsg"`
		Fail struct {
			Text string `json:"text"`
			Kind string `json:"kind"`
		} `json:"fail"`
		FailNoMsg struct {
			Text string `json:"text"`
			Kind string `json:"kind"`
		} `json:"failNoMsg"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("harness 输出非 JSON: %v\n%s", err, raw)
	}

	if !got.CNTodoHasButton || !got.CNTodoText || !got.CNTodoNoDoneTitle {
		t.Fatalf("CN 未签到单元不正确: %+v", got)
	}
	if !got.CNDoneText || !got.CNDoneHasTitle {
		t.Fatalf("CN 已签到单元必须显示「已签」+ 已签到提示: %+v", got)
	}
	if !got.GlobalHasNoButton {
		t.Fatal("国际版不得渲染可点「签到」按钮（上游恒 400，死入口）")
	}
	if !got.GlobalShowsNeutral {
		t.Fatal("国际版应显示中性「无签到」标记")
	}
	if !got.EntEmpty {
		t.Fatal("企业版不渲染签到单元")
	}

	if got.OK.Kind != "ok" || got.OK.Text != "签到完成，积分 252/550" {
		t.Fatalf("成功 toast = %q/%q", got.OK.Text, got.OK.Kind)
	}
	if got.OKWithMsg.Kind != "ok" || got.OKWithMsg.Text != "签到完成（今天已签到）" {
		t.Fatalf("成功附带消息 toast = %q/%q", got.OKWithMsg.Text, got.OKWithMsg.Kind)
	}
	if got.Fail.Kind != "err" {
		t.Fatalf("签到失败必须以 err 级别提示，实际 %q", got.Fail.Kind)
	}
	if got.Fail.Text != "签到未完成，积分 252：upstream client (http 400): 签到活动未开启或已过期" {
		t.Fatalf("失败 toast = %q", got.Fail.Text)
	}
	if got.FailNoMsg.Kind != "err" || got.FailNoMsg.Text != "签到未完成" {
		t.Fatalf("无消息失败 toast = %q/%q", got.FailNoMsg.Text, got.FailNoMsg.Kind)
	}
}
