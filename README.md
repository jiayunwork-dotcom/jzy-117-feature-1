# openchannel

长期挂载的明渠水力核算服务，基于 **Go 1.22 + 标准库 net/http**，单容器启动，无页面、无外部进程。

## 范围

- 明渠均匀流：矩形 / 梯形断面，已知流量按曼宁公式反解正常水深（返回流速、弗劳德数、流态、水力半径）。
- 薄壁矩形堰泄流：已知堰宽与堰上水头计算过堰流量；同时提供该公式的逆解（给定流量求堰上水头）。
- **渠线恒定渐变流水面线**：由若干首尾相接、断面/糙率/纵坡各异的渠段组成的整条渠道，
  在下游控制（薄壁堰或给定水深）下从下游往上游推求 M1 壅水 / M2 降水曲线；
  渠线与计算结果持久化、带版本历史，推求为异步作业。
- 明确**不做**：水跃（含陡坡段、曲线穿越临界水深的情形）、非恒定流、有压管网、任何前端页面。

## 数值方法、精度与代价

### 方程与推进方向

缓坡渠道的水面线由下游控制，采用**标准断面法（standard step）从下游向上游积分**能量方程。
相邻两站（d 为下游站、u 为上游站，渠底抬升 `S0·dx`）：

```
E(yu) = E(yd) + (Sf_mean − S0)·dx
E(y)  = y + Q²/(2g·A(y)²)
Sf_mean = (Sf(yd) + Sf(yu))/2          （隐式梯形格式）
```

隐式方程用二分法在**亚临界支（y > yc）**上求解：该支上残差严格单调
（`dE/dy = 1 − Fr² > 0`，Sf 随水深单调下降），不会多根、不会越过正常水深渐近线。

- **步长**：目标最大空间步长 **50 m**；每段等分为 `ceil(L/50)` 个区间，
  因此每段端点、渠段接缝、下游控制断面都恰好落在网格点上。
- **接缝处理**：相邻渠段在接缝处水深连续、不计局部水头损失（连续棱柱体渠道的标准 GVF
  处理）；各段渠底按自己的 `S0·L` 连续抬升，断面几何、糙率、纵坡在接缝处按段切换。
- **精度**：梯形空间格式的局部截断误差为 O(dx²)。以钉入回归测试的算例
  （b=3 m、n=0.015、S0=0.001、Q=5 m³/s、6 km 长矩形渠、3 m 宽 1.0 m 高 Cd=0.62 薄壁堰）
  为参照：堰前 1.939 m、上游 1 km 处 1.276 m、2 km 处 1.090 m，计算值与参考值
  偏差均在约 **0.0003 m（0.3 mm）**以内，远小于验收的 ±1 cm；3.5 km 以外与正常水深
  1.0787 m 的差小于 1 cm。
- **代价**：每一步只做 ~200 次标量二分求值（A、P、T 全部是闭式公式），
  6 km 渠道 120 步，毫秒级完成；代价随 `Σ L/50` 线性增长，无矩阵、无外部求解器。

### 失败状态（不处理水跃）

出现以下任一情况，作业以 `failed` 明确结束，并给出**渠段编号（0 = 最上游）和距下游控制
断面的距离（m）**，绝不返回跨过临界水深的曲线：

- `steep_reach`：某段 `yn < yc`（陡坡段，正常流态就是急流）；
- `critical_slope`：某段 `yn ≈ yc`（临界坡，无唯一亚临界曲线）；
- `critical_crossing`：某一步的根必须落在 `y ≤ yc`（曲线要穿过临界水深，例如上游窄断面
  的 yc 高于下游传来的水深）；
- `control_depth_bad`：下游给定水深本身不高于末段临界水深。

### 一套水力公式，不复制

- 断面 A、P、T、R 只有一份实现：`internal/geometry`。
- 正常水深只有一份：`internal/manning`。沿程摩阻坡降由已有曼宁关系导出：
  `Sf = S0·(Q / Q_manning(y))²`，不重写公式。
- 临界水深：`internal/flow` 新增一般梯形断面的 `CriticalDepth`（对已有 Fr/几何函数二分），
  矩形闭式解 `RectangularCriticalDepth` 保留为独立交叉校验。
- 堰的水头—流量关系只有一份：`internal/weir` 新增 `Head`，是 `Discharge` 的精确逆解，
  堰控水深 = 堰高 + `Head(Q)`。

## 存储选型与持久化

状态保存在 **SQLite**（`modernc.org/sqlite` 纯 Go 驱动，无 cgo，`CGO_ENABLED=0` 静态二进制，
与 golang:1.22 构建兼容），数据库文件位于挂载数据目录（默认 `/data/openchannel.db`，
WAL 模式）。选它的理由：

- 单容器部署，数据就是挂载卷上的一个文件，重启 / 重建容器后渠线、版本历史、作业、结果
  全部还在，无需另起数据库服务；
- 真事务：版本追加的乐观并发检查、作业终态与结果的同语句原子写入都靠事务/单语句保证；
- 部分唯一索引可直接表达「同一渠线同一版本至多一个活动作业」；
- 纯 Go 驱动让最终镜像不依赖 libc 工具链，构建方式与原来完全一致。

数据目录通过环境变量 `OPENCHANNEL_DATA` 配置（默认 `/data`），不存在时自动创建。

## 版本与并发

- 渠线不可变，每次修改（`PUT`）在该渠线上追加一个新版本；旧版本与旧结果都可查询。
- 修改必须带 `base_version`：若它已不是最新版本，返回 **409 Conflict**，不覆盖任何人的改动。
- 旧版本上已完成的结果不会被删除；在作业、版本和结果读取处都带 `stale`（版本号小于最新即为过期）。

## 作业语义

- `POST .../versions/{v}/profiles` 立即返回 `job_id`，可轮询 `GET /v1/jobs/{id}`
  查状态（`pending / running / succeeded / failed / cancelled / interrupted`）与进度
  （0~1，按已推进距离计），可 `POST .../jobs/{id}/cancel` 取消。
- **取消是协作式的**：积分器每一步检查一次取消信号；终态转换有状态守卫，
  取消与完成竞态时以先落库的终态为准，取消后不再写结果。
- **重启策略（显式中断，不静默续跑）**：进程启动时把所有遗留的 pending/running 作业标为
  `interrupted`。水面线积分无外部状态、单条渠道耗时仅毫秒到秒级，显式暴露中断状态、
  允许重新提交，比恢复一个工作进程已随旧进程消失的作业更可预期；且保证重启后作业不会
  永远挂在「进行中」。
- **重复提交**：同一渠线同一版本已有 pending/running 作业时，复用该作业（200 + `reused`）；
  已有成功结果时同样复用；`{"force":true}` 才另起新作业。同一版本的结果只有一个写者
  （数据库部分唯一索引），不会出现两份输出交错成半截水面线。
- 结果挂在渠线版本上，读取时带 `channel_id / version / stale / job_id`，能直接看出对应哪一版。

## 局部重算

缓流由下游向上游推进，改动只能影响「最下游被改渠段」以上游的部分。仅当以下条件同时
满足时做局部重算（否则自动回退为整条重算，回退对调用方透明）：

1. 设计流量不变；2. 下游控制完全不变；3. 两版定义存在一段非空的、渠段参数
   （长度、断面、糙率、纵坡）逐项相同的**公共尾段**（上游可改、可增、可删渠段）。

局部重算只积分尾段以上游的前缀，尾段站点逐浮点数复用；尾段渠段摘要按物理位置映射并
重排索引。局部重算与整条重算逐站一致（测试容差 1e-9，另有 API 级端到端测试盯住）。

重算完成后结果带 `comparison`：相对上一版水深变化最大的位置（距控制距离）与幅度
（new − old，带符号）、平均绝对变化与采样点数。

## 接口

原有两个接口的请求/响应格式保持不变：

### `POST /v1/uniform-flow`

```json
{"bottom_width":2.0,"side_slope":0.0,"roughness":0.02,"slope":0.001,"flow":3.0}
```

### `POST /v1/weir-flow`

```json
{"width":0.8,"head":0.25}            // discharge_coefficient 可省略，默认 0.62
```

### 渠线与版本

```sh
POST   /v1/channels                              # 登记渠线（创建即 v1）
GET    /v1/channels                              # 渠线 id 列表
GET    /v1/channels/{id}                         # 最新版本定义
PUT    /v1/channels/{id}                         # 追加新版本（body 带 base_version）
GET    /v1/channels/{id}/versions                # 版本历史
GET    /v1/channels/{id}/versions/{v}            # 指定版本定义（带 stale）
```

渠段按**上游到下游**的顺序给出，末段接下游控制。请求体：

```json
{
  "reaches": [
    {"name":"上段","length":6000,
     "section":{"bottom_width":3,"side_slope":0},
     "roughness":0.015,"slope":0.001}
  ],
  "design_flow": 5.0,
  "control": {"kind":"weir","width":3,"crest_height":1.0,"discharge_coefficient":0.62}
}
```

`control.kind` 取 `"weir"`（字段 `width/crest_height/discharge_coefficient`）或
`"depth"`（字段 `depth`，直接给定下游水深，m）。`PUT` 时在最外层加 `"base_version":1`。

### 水面线作业

```sh
POST /v1/channels/{id}/versions/{v}/profiles     # body 可空；{"mode":"partial"|"full","force":false}
GET  /v1/channels/{id}/versions/{v}/profile      # 该版本最近一次作业与结果（带 stale）
GET  /v1/jobs/{jobID}                            # 状态、进度、失败位置或结果
POST /v1/jobs/{jobID}/cancel                     # {"cancelled":true|false}
```

成功结果（`GET /v1/jobs/{id}` 中的 `result`）：

```json
{
  "profile": {
    "design_flow": 5.0,
    "downstream_depth": 1.939289,
    "total_length": 6000,
    "step_target_metres": 50,
    "upstream_end_at_normal_depth": true,
    "upstream_end_normal_depth_error": 1.96e-08,
    "points": [
      {"distance":0,"reach":0,"depth":1.939289,"velocity":0.859,"froude_number":0.241}
    ],
    "reaches": [
      {"index":0,"curve":"backwater","normal_depth":1.078661,"critical_depth":0.656663,
       "depth_upstream_end":1.078661,"depth_downstream_end":1.939289}
    ]
  },
  "recompute_mode": "full",
  "comparison": {"base_version":1,"max_delta_distance":6000,"max_delta_depth":0.0123,
                 "compared_length_metres":6000,"mean_abs_delta_depth":0.004,"sample_count":121},
  "job_id": "...", "computed_at": "..."
}
```

- `points[].distance` 为**距下游控制断面向上游的距离（m）**，从 0 开始递增；
  每个点带水深、流速、弗劳德数。
- `reaches[].curve` 为 `backwater`（壅水，M1）/ `drawdown`（降水，M2）/ `uniform`。
- 失败作业无 `result`，改为顶层 `failure: {kind, reach, distance, message}`。

### `GET /healthz`

返回 `{"status":"ok"}`。

## 预置算例（钉在回归测试里）

1. 单段矩形渠 b=3 m、n=0.015、S0=0.001、Q=5 m³/s、L=6 km，堰宽 3 m、堰高 1.0 m、Cd=0.62：
   堰前 1.939 m、1 km 处 1.276 m、2 km 处 1.090 m（±1 cm），3.5 km 以外与正常水深
   1.079 m 相差 < 1 cm；全程往上游单调下降且不低于正常水深。
2. 下游水深 = 正常水深时全程均匀流（偏差 < 1e-8）。
3. 缓坡段下游水深介于 yc 与 yn 之间为 M2 降水曲线，往上游抬升趋近 yn 且不越过。
4. 只调高堰高，任何位置水深不下降。
5. 两段渠只改上游段糙率：下游段逐点不变；局部重算与整条重算容差 1e-9。
6. 含陡坡段 / 会穿越临界水深的渠线：作业失败并报告渠段与距控制距离。
7. 旧版本提交修改被 409 拒绝；重启后渠线、历史、已完成结果可查；重启前未完成作业为
   `interrupted`，可重新提交。
8. 进行中作业可取消，取消后不产出结果；测试真实观察到进度推进且取消生效（步骤级钩子）。

## 本地运行与测试

```sh
go test ./... -race
go run ./cmd/server            # 默认监听 :8080；OPENCHANNEL_ADDR 改端口，OPENCHANNEL_DATA 改数据目录
```

## Docker

```sh
docker build -t openchannel .
docker run --rm -p 8080:8080 -v "$PWD/data:/data" openchannel
```
