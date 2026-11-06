package panel

// 「扣除积分审核」列的前端对抗测试：node + vm 切片，直接调 creditAuditCell。
// 脏数据 / XSS / 缺字段都不得抛异常，且不得输出未转义 HTML。

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestBndCreditAuditCell(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; credit audit cell test skipped")
	}
	f, err := os.CreateTemp(t.TempDir(), "credit-audit-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(bndCreditAuditScript); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("credit audit node test failed: %v\n%s", err, out)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &got); err != nil {
		t.Fatalf("node 输出不是 JSON: %v\n%s", err, out)
	}

	// 用户原例：记录扣 5、实际扣 10 → 显示 -5 且用危险色。
	if !strings.Contains(got["leak"], "-5") || !strings.Contains(got["leak"], "var(--bad)") {
		t.Fatalf("漏算应为 -5/红色: %s", got["leak"])
	}
	// 一致时是灰色 0，不能标红也不能标黄。
	if !strings.Contains(got["match"], ">0<") || !strings.Contains(got["match"], "var(--ink-3)") {
		t.Fatalf("一致应为灰色 0: %s", got["match"])
	}
	// 记录多于实际为正值且带 + 号，用警示色。
	if !strings.Contains(got["over"], "+3") || !strings.Contains(got["over"], "var(--warn)") {
		t.Fatalf("多算应为 +3/黄色: %s", got["over"])
	}
	// 缺字段 → —，不能编成 0。
	if !strings.Contains(got["missing"], "—") {
		t.Fatalf("无对账数据应显示 —: %s", got["missing"])
	}
	// 脏值不得抛出、不得把 NaN 渲染出来。
	if strings.Contains(got["dirty"], "NaN") || !strings.Contains(got["dirty"], "—") {
		t.Fatalf("脏值应回落为 —: %s", got["dirty"])
	}
	// 浮点噪声抹平为 0。
	if !strings.Contains(got["floatNoise"], ">0<") {
		t.Fatalf("浮点噪声应显示 0: %s", got["floatNoise"])
	}
	// title 里的昵称/数字不走 HTML 拼接，尖括号必须被转义。
	if strings.Contains(got["xss"], "<script>") || strings.Contains(got["xss"], "onerror=") {
		t.Fatalf("XSS 未转义: %s", got["xss"])
	}
}

const bndCreditAuditScript = `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const escStart = src.indexOf('function esc(');
const escEnd = src.indexOf('function ago(');
const start = src.indexOf('function creditAuditCell(');
const end = src.indexOf('function renderCreditDim(');
if ([escStart, escEnd, start, end].some(v => v < 0)) throw new Error('credit audit helpers not found');
const ctx = { Number, String, Math, isFinite };
vm.createContext(ctx);
vm.runInContext(src.slice(escStart, escEnd) + src.slice(start, end) +
  '\nthis.creditAuditCell=creditAuditCell;', ctx);
const out = {};
const run = (k, row) => { try { out[k] = ctx.creditAuditCell(row); } catch (e) { out[k] = 'THREW:' + e; } };
run('leak', { credits: 5, ledger_spent: 10, audit_diff: -5 });
run('match', { credits: 7, ledger_spent: 7, audit_diff: 0 });
run('over', { credits: 10, ledger_spent: 7, audit_diff: 3 });
run('missing', { credits: 4 });
run('dirty', { credits: 'x', ledger_spent: 'y', audit_diff: 'abc' });
run('floatNoise', { credits: 3, ledger_spent: 2.9999999, audit_diff: 0.0000001 });
run('xss', { credits: 1, ledger_spent: 2, audit_diff: -1, nickname: '<script>x</script>' });
process.stdout.write(JSON.stringify(out));`
