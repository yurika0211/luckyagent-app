# Agent Eval（第一刀）

`la-eval` 评测走真实 Agent loop 的用例。`cmd/la-*-bench` 仍是组件级确定性评测，互不替代。

## 用例

样品：`eval/testdata/cases.json`。一份 schema，单轮是长度为 1 的 `turns`，多轮把字段摊开。

每轮合同固定为 `input` / `expected` / `evaluation` / `wait`。`evaluation` 可组合：

- `keyword`：`all` 必须都出现，`any` 至少出现一个。
- `regex`：回复必须匹配全部 pattern。
- `trace`：`required` 工具必须出现，`forbidden` 工具出现即失败。
- `memory_write`：必须观察到写入，且写入的是更正后的值。

未配置的验证器是跳过。证据不足是未知。两者都不算通过。整组所有有效轮通过才算通过。

多轮只允许 `replay_abort`、`replay_resume`、`inject_abort`。`inject_resume` 直接拒绝。`replay_resume` 在失败后续跑时必须把补写前缀交到下一轮，只写在记录里不算证据。

和现有 bench JSONL 对不上的列见 `eval/testdata/bench-jsonl-mismatch.md`。旧列导入会被拒绝。

## 运行

```bash
go run ./cmd/la-eval validate -dataset eval/testdata/cases.json
go run ./cmd/la-eval create-run -dataset eval/testdata/cases.json
go run ./cmd/la-eval start -run RUN_ID
go run ./cmd/la-eval report -run RUN_ID -compare BASE_RUN_ID
go run ./cmd/la-eval ui -addr 127.0.0.1:3910 -runs-dir "$LUCKYAGENT_EVAL_HOME"
```

`la-eval ui` 是评测操作面，分测试资产、执行与证据、对照、人工复核四块：

- 粘贴用例 JSON 后校验。旧 bench 字段、`inject_resume`、缺字段都会拒绝。未配置的验证器标成 skip。
- 建档只冻结清单，请求里带 `start=true` 会被拒绝。页面上的开跑必须勾选确认，并且只接受 scripted 回放。真实 Agent loop 仍用 `la-eval start -executor agent`。
- 详情把冻结清单、execution、裁判原判、复核结论分开。每轮展示输入、期望、回复、工具名和参数、记忆写入、四个验证器的 pass/fail/skip/unknown 及原因，以及补写是否真正送到下一轮。JSONL 全部尝试和报告采用的最新一条可切换。
- 人工复核必须有备注，只写 `reviews.json`，不改 `attempts.jsonl`。HTML 报告同时显示原判和复核。
- 对照页列出新增失败、恢复、新增用例和消失用例，并标明两次 run 的 dataset sha 是否相同。
- 校验失败、未勾选确认、脚本路径错误、run 不存在、没有 attempts，都写在页面上。

`create-run` 默认不执行。只有显式 `-start`，或之后单独 `start`，才会跑。清单冻结在该 run 的 `manifest.json`。改用例文件后，旧 run 的报告仍用旧清单，新 run 用新清单。

`-executor agent` 走真实 loop，需要操作者配置里的 provider。`-executor scripted -script FILE` 只回放准备好的证据，用来核对冻结、判定和报告，不调用模型。

结果同时有 `execution`（completed / failed / unknown）和 `verdict`（pass / fail / skip / unknown）。`attempts.jsonl` 保留全部尝试，HTML 报告取每个 case 的最新一条，并列出相对基线 run 的新增失败与恢复。

评测数据在 `$LUCKYAGENT_EVAL_HOME` 或 `~/.luckyagent-eval`，不写被测的 `~/.luckyagent`。

## 还不在第一刀里

飞书表格、MQTT、缺陷工单、多租户、SWE-bench 替换、CI 门禁、LLM judge、把裁判提示词写入生产 memory prompts。
