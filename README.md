# openchannel

长期挂载的明渠水力核算服务，基于 **Go 1.22 + 标准库 net/http**，单容器启动，无页面、无第三方运行时依赖。

## 范围

- 明渠均匀流：矩形 / 梯形断面，已知流量按曼宁公式反解正常水深（返回流速、弗劳德数、流态、水力半径）。
- 薄壁矩形堰泄流：`Q = (2/3)·Cd·b·sqrt(2g)·H^(3/2)`，以及它的反函数（给定流量求堰上水头）。
- **渠线（多渠段首尾相接）与恒定渐变流水面线推求**：从下游控制（薄壁堰或给定水深）向上游积分，给出沿程水深、流速、弗劳德数、各段壅水/降水判定、渠首是否回到正常水深；以异步作业方式执行，渠线带版本，结果挂版本并持久化。
- 明确**不做**：水跃（遇到陡坡段或积分中要穿过临界水深时作业明确失败）、非恒定流、有压管网、任何前端页面。

## 水面线数值方法（选型与精度代价）

- 方程：标准恒定渐变流方程 `dy/dx = (S0 − Sf)/(1 − Fr²)`，x 向下游。服务内部 ξ 从**下游控制断面向上游**计（ξ = 0 在控制断面），故积分形式为
  `dy/dξ = (Sf − S0)/(1 − Fr²)`。
- 单步方法：**经典四阶 Runge–Kutta（RK4）**。基准步长 **20 m**（短于 20 m 的渠段单步走完），出报点即积分节点，读取无需插值。
  - 精度：对题给算例做 20 m 对 10 m 的半步加密对比，各抽样点水深差 **< 2×10⁻⁶ m**（有回归测试 `TestStepRefinement` 盯住）；与参考值 1.939 / 1.276 / 1.090 m 相比误差在毫米量级。
  - 代价：每步 4 次右端项求值，6 km 渠段约 1200 次几何/曼宁求值，亚毫秒级完成；存储上每 20 m 一个点（6 km 约 300 点）。
- 自适应保护：单步出现非有限结果、弗劳德数逼近 1（Fr ≥ 0.999）或跨过临界水深、或一步从正常水深一侧跳到另一侧时，步长反复减半（最小 5 cm）；减到最小仍要穿临界水深则作业判失败。接近正常水深渐近线时以正常水深本身收步（导数在该处趋于 0），避免渐近区无效细分。
- 渠段接缝：假定接缝处渠底连续、不另计局部收缩/扩大损失；**水深连续**，流速与弗劳德数用进入渠段自己的断面重新计算。不同断面、糙率、纵坡的渠段只是一组新的 yn/yc 与右端项，积分器不变。
- 不做水跃：开推前先对每段用现有求解器算 yn（曼宁反解）与 yc（临界水深）。`yn < yc` 的陡坡段、`yn ≈ yc` 的临界坡段直接判失败；缓流上推过程中逼近 yc 同样判失败，并给出**渠段序号与距控制断面的距离**。
- 下游控制：堰控时用堰流公式的**反函数** `weir.Head` 求堰上水头，`水深 = 水头 + 堰高`；水深控制直接给定。两者都复用既有公式，水面线包内不存在第二份堰流/曼宁公式。

## 只有一套水力学实现（防双份求解器）

- 断面 A、P、T、R 全部来自 `internal/geometry`。
- 正常水深 `manning.NormalDepth`（二分）与摩阻坡降 `manning.FrictionSlope`（曼宁公式反解 Sf）同在 `internal/manning`，是同一条 Q–Sf 关系。
- 临界水深 `flow.CriticalDepth` 解 `Q²T = gA³`（通用梯形，二分），矩形时与闭式解 `RectangularCriticalDepth` 一致（有对拍测试）。
- 堰上水头 `weir.Head` 是 `weir.Discharge` 的严格反函数。
- 水面线引擎 `internal/profile` 只调用上述函数，不复制任何公式。

## 存储选型与持久化

使用**嵌入式 JSON 文件存储**（`internal/store`，零第三方依赖），每个渠线、每个作业一个文件，写入采用「同目录临时文件 → fsync → rename 原子替换」，读者永远看不到写了一半的文档。

理由：

1. 负载是单容器、少量渠线与偶发计算，没有多写者与吞吐压力；进程内一把读写锁即可，比引入数据库和迁移简单。
2. 零第三方依赖：构建不需要模块代理；二进制保持 `CGO_ENABLED=0`（现有 Dockerfile 即如此，引入 cgo 版 SQLite 会破坏这一构建）；数据文件可读、可直接备份。
3. 数据目录通过挂载持久化：容器默认 `OPENCHANNEL_DATA_DIR=/data`（Dockerfile 声明了 `VOLUME ["/data"]`），服务重启或容器重建后渠线、全部版本历史、作业与已完成结果都还在。

渠线文档结构：`data/channels/<id>.json`（内含不可变版本数组）；作业：`data/jobs/<id>.json`。

## 版本与并发修改

- 每次修改渠线都**新增一个不可变版本**（版本号在渠线内单调递增，从 1 开始），旧版本永久可查。
- 修改请求必须带 `base_version`（乐观锁）：它不等于当前最新版本时返回 **409 `version_conflict`**，不会悄悄盖掉别人的改动。
- 渠线出现新版本后，旧版本上的成功结果读取时标记 `"stale": true`，可对新版本重新提交推求。

## 异步作业语义

- `POST .../profile-jobs` 立即返回作业标识；`GET /v1/jobs/{id}` 查状态（`queued/running/succeeded/failed/canceled/interrupted`）与进度（0~1）；`POST .../jobs/{id}/cancel` 取消。
- 单 worker 串行执行队列：同一条渠线同一版本**不会并发推两次**，两份输出不可能交错写进同一份结果（结果只在作业终态一次性原子写入）。
- 重复提交的幂等规则（可预期）：
  - 已有 `queued/running` 作业 → 返回该作业（`reused: true`）；
  - 该版本已有未过期成功结果 → 返回原作业与原结果（`reused: true`）；
  - 结果已过期、或上次失败/取消/中断 → 新建作业。
- **重启语义**：不做断点续算（渐变流推求亚毫秒级，续算所需的检查点代价比重算大）。进程重启时，启动恢复把所有仍处于 `queued/running` 的作业一律置为终态 **`interrupted`**，说明原因，允许重新提交；作业不会永远挂在「进行中」。优雅停机（SIGTERM）时在跑的作业置为 `canceled`。
- 取消：排队中的作业同步取消；运行中的作业在**下一个积分步**生效，取消后不产出任何结果。

## 重算策略与新旧对比

- 只实现**整条重算**，不做局部重算：缓流虽自下游向上推、改上游段天然不影响下游段（测试验证下游段水深差为 0），但局部重算要额外维护「受影响起点」并证明与整条重算一致，而整条重算本身是毫秒级，收益不抵复杂度。
  等价地，验收五关注的「下游段不变」由数学性质保证，并由 HTTP 回归测试逐点钉住（20 m 网格，容差 1e-6 m）。
- 每次新版本成功后自动生成与上一版结果的对比概要：水深最大升高、最大降低、**最大变幅及其位置（距控制断面距离）**、平均变幅；多段渠线另报最下游段的最大变幅。

## 接口

原有两个接口的请求与响应格式**保持不变**：

- `POST /v1/uniform-flow`
- `POST /v1/weir-flow`
- `GET /healthz`

新增：

| 方法与路径 | 说明 |
|---|---|
| `POST /v1/channels` | 登记渠线（有序渠段、设计流量、下游控制），得到 id 与版本 1 |
| `GET /v1/channels` | 列出全部渠线（id、名称、当前版本号、版本数） |
| `GET /v1/channels/{id}` | 渠线与完整版本历史 |
| `GET /v1/channels/{id}/versions/{n}` | 某一版本定义（含 `is_current`、`result_stale`） |
| `POST /v1/channels/{id}/versions` | 基于 `base_version` 提交新版本；过期返回 409 |
| `POST /v1/channels/{id}/versions/{n}/profile-jobs` | 提交水面线推求（立即返回作业） |
| `GET /v1/jobs/{id}` | 作业状态、进度、失败定位 |
| `POST /v1/jobs/{id}/cancel` | 取消作业（终态作业返回 409） |
| `GET /v1/channels/{id}/versions/{n}/profile-result` | 挂在该版本上的结果（含 `stale` 与对比概要） |

登记渠线请求示例：

```json
{
  "name": "trunk",
  "flow": 5.0,
  "reaches": [
    {
      "name": "upper",
      "length": 3000.0,
      "section": {"bottom_width": 3.0, "side_slope": 0.0},
      "roughness": 0.015,
      "slope": 0.001
    }
  ],
  "control": {
    "type": "weir",
    "weir": {"width": 3.0, "crest_height": 1.0, "discharge_coefficient": 0.62}
  }
}
```

控制也可给水深：`{"type": "depth", "depth": 1.2}`。`discharge_coefficient` 省略时取 0.62。

结果中的每个沿程点：

```json
{
  "distance_from_control_m": 1000.0,
  "reach_index": 0,
  "reach_name": "upper",
  "depth_m": 1.2761,
  "velocity_mps": 1.3061,
  "froude_number": 0.3691,
  "at_reach_boundary": false
}
```

渠段结果含 `normal_depth_m`、`critical_depth_m`、`slope_class`（mild/steep/critical）、
`curve_type`（backwater 壅水 / drawdown 降水 / uniform）、`normal_depth_reached_distance_m`；
渠线结果含 `upstream_end.at_normal_depth`（渠首是否在正常水深 1 cm 带内）与
`backwater_extent_distance_m`（壅水影响到达的最远距离）。

水力学失败的作业（示例）：

```json
{
  "status": "failed",
  "failure": {
    "code": "critical_depth_crossing",
    "reach_index": 0,
    "reach_name": "upper",
    "distance_from_control_m": 871.48,
    "message": "profile reaches the critical depth ..."
  }
}
```

失败码：`steep_reach`、`critical_slope_reach`、`critical_depth_crossing`、
`control_depth_not_subcritical`。

## 输入校验（计算前拦截，HTTP 400）

渠段长度/底宽/糙率/纵坡为正、边坡非负、设计流量为正、至少一段；堰宽与流量系数为正、堰高非负；
水深控制水深为正；`control.type` 必须是 `weir` 或 `depth`；修改必须带正的 `base_version`。
水力学上不可行（陡坡、穿临界）不是 400，而是作业 `failed` 并定位。

## 预置算例（钉在回归测试里）

单段矩形渠 b = 3 m，n = 0.015，S0 = 0.001，Q = 5 m³/s，长 6 km，下游薄壁堰
b = 3 m、堰高 1.0 m、Cd = 0.62：

- 堰前水深（ξ = 0）≈ **1.939 m**
- ξ = 1 km ≈ **1.276 m**，ξ = 2 km ≈ **1.090 m**
- 正常水深 yn ≈ **1.079 m**；3.5 km 以外与 yn 相差 < 1 cm
- 全程水深向上游单调不增且不低于 yn（M1 壅水曲线）

这些数值同时钉在引擎级测试（`internal/profile`）和 HTTP 端到端测试（`internal/server`）里，
容差 ±1 cm。另有：yn 控制恒均匀、M2 降水趋近 yn 不过冲、抬高堰顶处处水深不降、
改上游段糙率下游段不变（1e-6 m）、陡坡/穿临界失败定位、乐观锁冲突、重启持久化与中断恢复、
进度可观察且取消生效、重复提交复用等自动化测试。

## 本地运行与测试

```sh
go test ./...
go test -race ./...
OPENCHANNEL_ADDR=:8080 OPENCHANNEL_DATA_DIR=./data go run ./cmd/server
```

未设置数据目录时默认 `/data`；若该目录不可写（本机裸跑），自动退回临时目录并在日志中说明。

## Docker

```sh
docker build -t openchannel .
docker run --rm -p 8080:8080 -v "$PWD/data:/data" openchannel
```

数据目录 `/data` 在镜像中声明为卷；挂载它即可在容器重建后保留渠线、版本、作业与结果。
