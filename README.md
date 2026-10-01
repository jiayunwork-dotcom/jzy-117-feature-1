# openchannel

长期挂载的明渠水力核算服务，基于 **Go 1.22 + 标准库 net/http**，单容器启动，无页面、无外部依赖。

## 范围

- 明渠均匀流：矩形 / 梯形断面（矩形即边坡 `side_slope = 0`），已知流量按曼宁公式反解正常水深，并返回流速、弗劳德数、流态、水力半径。
- 薄壁矩形堰泄流：已知堰宽与堰上水头，按 `Q = (2/3)·Cd·b·sqrt(2g)·H^(3/2)` 计算过堰流量。
- 明确**不做**：有压管网、流域产汇流、任何前端页面。

## 自洽性

面积 `A`、湿周 `P`、水面宽（顶宽）`T` 只有一套实现，位于
`internal/geometry`。曼宁迭代用的 `A`、`R` 与判流态用的水面宽 `T`
全部来自同一断面几何函数，矩形/梯形不存在两份求解器。

- 曼宁公式（SI）：`Q = (1/n)·A·R^(2/3)·sqrt(S0)`，`R = A/P`。
- 弗劳德数：`Fr = V / sqrt(g·A/T)`，`V = Q/A`。
- 矩形临界水深闭式解：`yc = (Q²/(g·b²))^(1/3)`，用于独立抽查流态判定。
- 流量随水深严格单调，正常水深用二分法（先倍增括号再二分）求解。
- 堰上水头以堰顶为基准，与渠道正常水深没有自动换算关系；堰接口只在
  `note` 字段提示需要统一基准面，服务不会改动任何公式去迎合比较结果。

## 输入校验（计算前拦截，HTTP 400）

糙率非正、渠底坡度非正、底宽非正、边坡为负、流量为负、堰宽非正、
堰上水头非正、流量系数非正，均返回错误并标明字段。

## 接口

### `POST /v1/uniform-flow`

```json
{
  "bottom_width": 2.0,
  "side_slope": 0.0,
  "roughness": 0.02,
  "slope": 0.001,
  "flow": 3.0
}
```

响应：

```json
{
  "normal_depth": 1.3677515051663782,
  "velocity": 1.0966904399915354,
  "froude_number": 0.29939597210037816,
  "regime": "subcritical",
  "hydraulic_radius": 0.5776583827238528,
  "area": 2.7355030103327564,
  "top_width": 2.0
}
```

`flow = 0` 合法，返回零水深与 `regime = "no_flow"`。

### `POST /v1/weir-flow`

```json
{ "width": 0.8, "head": 0.25 }
```

`discharge_coefficient` 可省略，默认 `0.62`（要求时必须为正）。

```json
{
  "flow": 0.1830838059468942,
  "width": 0.8,
  "head": 0.25,
  "discharge_coefficient": 0.62,
  "note": "weir head H is referenced to the weir crest and is not a channel normal depth; compare them only after converting to a common datum"
}
```

### `GET /healthz`

返回 `{"status":"ok"}`。

## 预置算例（钉在回归测试里）

矩形渠 `b = 2 m`，`n = 0.02`（SI），`S0 = 0.001`，`Q = 3 m³/s`：

- 正常水深 `yn ≈ 1.3678 m`（手算量级：A≈2.74，R≈0.578，
  `50·2.74·0.694·0.03162 ≈ 3.0`）
- 代回曼宁公式收回 `Q = 3`（相对误差 < 1e-9）
- 临界水深 `yc ≈ 0.6121 m`，`yn > yc`，缓流/亚临界
- 坡度加大（0.001→0.002→0.005→0.01）时同一流量的正常水深单调下降

堰 `b = 0.8 m`，`H = 0.25 m`，`Cd = 0.62`：`Q ≈ 0.1831 m³/s`。

## 本地运行与测试

```sh
go test ./...
go run ./cmd/server            # 默认监听 :8080，可用 OPENCHANNEL_ADDR 覆盖
```

## Docker

```sh
docker build -t openchannel .
docker run --rm -p 8080:8080 openchannel
```
