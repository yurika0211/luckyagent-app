# la-*-bench JSONL 对不上 agent-eval/v1 的列

核对时间：2026-11-03。来源是本仓库 `cmd/la-*-bench` 的记录结构，不是某一次跑出来的 JSONL 文件。

`agent-eval/v1` 的用例合同只有 `input` / `expected` / `evaluation` / `wait`。下面这些列出现在组件 bench 的 JSONL 里，不能当作 case 字段导入。`la-eval validate` 遇到它们会直接拒绝。

组件 bench 仍独立运行。本表只说明为什么它们的输出不能当成真实 Agent loop 的冻结清单。

## 用例合同里没有对应列

| bench JSONL 列 | 出现在 | 对不上的原因 | agent-eval/v1 落点 |
| --- | --- | --- | --- |
| `type` | 全部 la-*-bench | 一条 JSONL 里混着 record 和 summary | 用例文件没有记录类型；run 用 manifest / attempts / report 分开 |
| `variant` | tool, memory, cache, graph-rag, context-packer, multiagent | 策略对照标签 | run 的 `profile`（chat / long_task），不是用例字段 |
| `scenario` | tool, memory, cache, graph-rag, context-packer, multiagent | 组件场景名 | `case.coverage`：chat / tool_route / memory_write / long_task |
| `task_id` | tool, multiagent | 组件任务编号 | `case.id` |
| `task_type` | tool, multiagent | 组件任务类型 | `case.coverage` |
| `round` | tool, memory, rag, graph-rag, cache, context-packer, multiagent | 同一题重复次数 | run 里同一 case 的多次 attempt；报告只取最新一条 |
| `prompt` | tool, cache, multiagent | 单轮提示 | `turns[].input` |
| `query` | memory, rag, graph-rag, context-packer | 检索问句 | `turns[].input` |
| `query_id` | memory, rag, graph-rag, context-packer | 检索样本编号 | `case.id` |
| `required_tools` | tool, memory | 路由金标，写在记录上 | `turns[].evaluation.trace.required` |
| `forbidden_tools` | tool | 同上 | `turns[].evaluation.trace.forbidden` |
| `required_operations` | tool | 操作级路由，组件专用 | 无对应列，导入拒绝 |
| `forbidden_operations` | tool | 同上 | 无对应列，导入拒绝 |
| `visible_tools` | tool | 当次可见工具面 | 属于 profile / 运行配置，不是用例字段 |
| `called_tools` | tool | 当次调用结果 | attempt 的 `evidence.tools`，不是用例字段 |
| `called_operations` | tool | 操作级调用结果 | 无对应列 |
| `tool_calls` | tool；cache 用同名整数计数 | 证据或计数 | attempt 的 `evidence.tools` |
| `success` | tool, multiagent | 执行成功和质量通过混成一个布尔 | `attempt.execution` 与 `attempt.verdict` 分列 |
| `clean` | tool, memory, rag, graph-rag, context-packer, multiagent | 组件阈值汇总 | 不用；未配置或证据不足是 skip / unknown |
| `quality_pass` | memory, graph-rag, context-packer | 组件质量布尔 | `turns[].evaluation` 的组合验证器 |
| `recall_at_k` | memory | 检索指标 | 无对应列 |
| `precision_at_k` | memory | 检索指标 | 无对应列 |
| `mrr` | memory, rag(`metrics.mrr`) | 检索指标 | 无对应列 |
| `ndcg_at_k` | memory | 检索指标 | 无对应列 |
| `risk_flags` | memory | 组件风险标记 | 无对应列 |
| `size` | memory | 合成数据集大小 | 无对应列 |
| `seed` | 组件合成入口的配置，不进 v1 用例 | 随机种子 | 无对应列 |
| `started_at` | tool, memory, rag 相邻记录, cache, graph-rag, context-packer, multiagent | 执行时刻 | attempt 元数据 |
| `duration_ms` | 全部记录型 bench | 耗时 | attempt 元数据 |
| `duration_ns` | tool, memory, rag, graph-rag, context-packer, multiagent | 耗时 | attempt 元数据 |

## 样品里摊开、bench JSONL 没有的列

| agent-eval/v1 字段 | bench JSONL |
| --- | --- |
| `schema_version` | 无。bench 用 `type=record/summary` 区分流 |
| `cases[].turns[]` | 无多轮合同。bench 一条记录是一轮合成题 |
| `turns[].expected` | 无。bench 把金标拆进 `required_tools`、`need_tool_gold`、`expected_sources` |
| `turns[].evaluation.keyword` | 无 |
| `turns[].evaluation.regex` | 无 |
| `turns[].evaluation.memory_write` | 无。memory bench 记的是召回，不是「是否写入更正后的值」 |
| `turns[].wait` | 无。bench 的 `sleep_before_next_ms` 是压测间隔，不是用例合同 |
| `case.mode` = `replay_abort` / `replay_resume` / `inject_abort` | memory/rag/graph-rag 的 `mode` 是检索策略，不是这三种跑法 |
| `attempt.execution` + `attempt.verdict` | 只有 `success` 或 `quality_pass` |
| `attempt.addendum` | 无。bench 不会在失败后续跑并留下补写证据 |
| run `dataset_sha256` | 无。bench JSONL 不冻结题目版本 |

## 不导入、也不映射的列

这些列留在各自 bench 里，平台第一刀不接收：

- tool：`need_tool_gold`、`need_tool_prob`、`need_prediction_correct`、`tool_recall`、`tool_precision`、`operation_recall`、`operation_precision`、`redundant_rate`、`route_risk`、`tool_tune_score`、`info_gain`、`success_probability`
- memory：`graph_enabled`、`update_access`、`stale_hit_rate`、`noise_at_k`、`forbid_hit_count`、`memory_dir`
- rag：`hit_at_1`、`recall_at_1`、`inner_iterations`、`sources`、`rank`
- graph-rag：`source_recall`、`node_recall`、`rel_recall`、`expected_nodes`、`expected_rels`、`result_nodes`
- cache：`cached_prompt_tokens`、`cached_ratio`、`stable_prefix_ratio`、`prompt_tokens`、`system_prompt_hash`
- context-packer：`critical_memory_retention`、`constraint_retention`、`temporal_warning_retention`、`bucket_tokens`、`token_efficiency`
- multiagent：`gold_mode`、`predicted_mode`、`subtask_recall`、`capability_recall`、`speedup`、`multi_agent_score`
- proactive：独立 main，不产出可导入的 case 清单

`la-swe-bench` 仍是端到端 harness，不由本 schema 替换。
