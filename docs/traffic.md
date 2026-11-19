# 流量策略与本地账本

在 Linux 上，通过 `--traffic-config`、`AGENT_TRAFFIC_CONFIG` 或 Agent JSON 的 `traffic_config` 字段指定本地策略文件。省略该配置时，Agent 沿用原生网卡筛选、速率和累计逻辑，以及 `--month-rotate` 对应的 `net_static.json`。

启用策略时，`--include-nics` 必须列出完整网卡名称。沿用 `--exclude-nics`，排除优先。可显式选择隧道或回环接口。启动时要求所有选中的接口存在。运行时发现缺卡会通过现有上报的 `message` 字段诊断，其他接口继续计量，缺卡恢复后重新建基线。采集使用 Linux boot ID 和接口 ifindex/MAC，`HOST_PROC` 须留空或设为 `/proc`。

## 策略示例

```json
{
  "version": 1,
  "state_file": "/var/lib/komari-traffic/state.json",
  "report": "billing",
  "flush_seconds": 60,
  "weights": {
    "nic-a": {"rx_to_in_ppm": 1000000, "tx_to_out_ppm": 1000000},
    "nic-b": {"rx_to_out_ppm": 500000, "tx_to_out_ppm": 2000000}
  }
}
```

以 `--include-nics nic-a,nic-b` 为例，计费入站增量为 `nic-a RX`，计费出站增量为 `nic-a TX + 0.5 × nic-b RX + 2 × nic-b TX`。替换为实际接口名称后使用。每个倍率以 ppm 整数表示，`1000000` 为 1 倍，允许值为 `0` 至 `1000000000000`。有权重对象的网卡，省略的方向权重为 0。没有权重对象的网卡使用原始方向，RX→入站和 TX→出站各为 1 倍。

倍率按每次采样的增量计算，每网卡、每方向的不足一字节余量写入账本。运行时修改策略文件需要重启。策略变化后从新基线开始，已有累计及余量保留。启用策略期间，后台自更新和面板触发的二进制换版都会停用，以保留该计量能力。

| 上报字段 | 启用策略时的口径 |
| --- | --- |
| `up`、`down` | 选中网卡分别按原始 TX、RX 增量除以实际采样间隔，单位为字节/秒 |
| `totalUp`、`totalDown` | `report: "raw"`（默认）上报真实出站、入站字节；`report: "billing"` 上报加权出站、入站字节 |

启用 `--month-rotate` 时，上报累计采用 `netstatic` 原有的重置日算法和包含边界的时间窗口，增量归属采样结束时刻。原始和加权窗口记录与终身累计同写一份账本。月窗口过期不影响终身累计。

## 本地诊断和初始化

```sh
./komari-agent traffic-check --traffic-config ./traffic-policy.json --include-nics nic-a,nic-b
./komari-agent traffic-status --traffic-config ./traffic-policy.json --include-nics nic-a,nic-b
```

`traffic-check` 输出选卡、权重和当前物理计数，缺卡时同时返回非零退出码。`traffic-status` 读取最近一次落盘的快照，包含全局和逐网卡原始与加权累计、余量、基线、月窗口和最近事件。两条命令都是只读的。本地子命令读取流量相关参数或环境变量，独立于 Agent 的 `--config`、认证和面板连接。

使用 `traffic-seed` 创建一份新账本。四个累计值均必填，新实例可显式传 0。迁移现有累计时，填入同一时刻、对应口径的已上报字节数。使用 `--month-rotate` 时，同时传入相同的重置日与当前窗口的四个种子值 `--period-raw-in`、`--period-raw-out`、`--period-billing-in`、`--period-billing-out`。如需变更 `report` 或重置日，使用新账本和对应口径的种子。

```sh
./komari-agent traffic-seed --traffic-config ./traffic-policy.json --include-nics nic-a,nic-b \
  --raw-in 0 --raw-out 0 --billing-in 0 --billing-out 0
```

账本需要绝对路径和专用目录，目录权限为 `0700`、文件权限为 `0600`。首次基线、boot ID 或策略变化会立即落盘，也会在正常退出时写盘。常规落盘间隔由 `flush_seconds` 指定，默认 60 秒，有效范围为 10 至 3600 秒。程序用独占文件锁保护写入，并用校验和检测意外损坏。原子替换前写入失败时，累计冻结在上次接受的值。如果替换成功后目录同步失败，程序保留已可读取的快照并停止继续计量，通过 `message` 报告持久化错误。

同一系统、同一策略下，Agent 重启可从账本基线继续计算。观察到系统重启、网卡身份变化或网卡消失重现时重新建基线。单方向计数变小时，该方向的本次增量归零。采样随原有上报周期运行，连接中断期间的累计可在下次采样补入，前提是计数仍可比较。两次采样之间无法观察到的计数器重置无法恢复。
