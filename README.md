# SRS Enhanced RTMP 动态拉流方案

这是一个轻量的 IPTV 组播/UDP/RTP 转 RTMP 方案：

- SRS 6 负责对外提供 RTMP，并支持 HEVC/H.265 Enhanced RTMP。
- 单文件 Go puller 接收 SRS 的 `on_play` / `on_stop` 回调。
- 客户端开始播放时才启动 FFmpeg 拉源。
- 默认最多同时拉 3 个不同频道。
- 同一个频道多个客户端观看时只启动 1 个 FFmpeg。
- 无人观看后默认 5 秒停止 FFmpeg。

## 地址格式

只支持 SRS-safe 格式：

```text
rtmp://192.168.10.20:1935/udp/239_254_97_96_8550
rtmp://192.168.10.20:1935/rtp/239_69_1_27_9802
```

stream key 必须是 5 段全下划线格式：

```text
239_254_97_96_8550
```
## 拉源规则

如果配置了 `UDPXY_BASE_URL`：

```text
rtmp://192.168.10.20:1935/udp/239_254_97_96_8550
```

会拉：

```text
http://192.168.10.1:4000/udp/239.254.97.96:8550
```

如果配置了 `UDPXY_BASE_URL`：

```text
rtmp://192.168.10.20:1935/rtp/239_254_97_96_8550
```

会拉：

```text
http://192.168.10.1:4000/rtp/239.254.97.96:8550
```

如果没有配置 `UDPXY_BASE_URL`，puller 会直接从组播拉流：

```text
udp://@239.254.97.96:8550
rtp://@239.254.97.96:8550
```

## 部署

先按机器情况修改 `docker-compose.yml`：

```yaml
SRS_RTMP_BASE_URL: "auto"
UDPXY_BASE_URL: "192.168.10.1:4000"
MAX_STREAMS: "3"
```

说明：

- `SRS_RTMP_BASE_URL: "auto"` 表示 FFmpeg 会按播放器请求里的 SRS `tcUrl` 回推，避免 SRS vhost 不一致。
- `UDPXY_BASE_URL` 有值时走 udpxy/http；为空时直接拉 `udp://@...` 或 `rtp://@...`。
- `MAX_STREAMS` 限制最多同时拉几个不同频道，性能弱的 NAS 建议保持 `3` 或更小。

启动：

```sh
docker compose up -d --build
```

停止：

```sh
docker compose down
```

## 网络模式

默认设计：

- SRS 使用 Docker bridge 网络，只映射必要端口。
- puller 使用 host 网络，方便直接接收组播，也方便访问宿主机网络。

如果 SRS bridge 容器访问 puller 的 `18090` 超时，需要放行 Docker bridge 到宿主机的 hook 端口：

```sh
iptables -I INPUT -i docker0 -p tcp --dport 18090 -j ACCEPT
```

如果服务器有 1Panel 或其他防火墙，这条规则可能需要做持久化。

## 推流安全

SRS 已启用 `security` 规则：

- 允许所有地址拉流。
- 只允许内网地址推流。
- 不在允许列表里的地址默认不能推流。

允许推流的网段：

```text
127.0.0.1
10.0.0.0/8
172.16.0.0/12
192.168.0.0/16
```

说明：`172.16.0.0/12` 是标准私有网段，也覆盖 Docker 默认的 `172.17.0.0/16`。

## 常用命令

看容器：

```sh
docker ps
```

看日志：

```sh
docker logs -f srs-enhanced-rtmp
docker logs -f srs-enhanced-rtmp-puller
```

看 puller 状态：

```sh
curl http://127.0.0.1:18090/streams
```

健康检查：

```sh
curl http://127.0.0.1:18090/health
```

## 编码策略

视频始终复制：

```text
-c:v copy
```

这样 CPU 占用最低，也能让 HEVC/H.265 通过 SRS 6 的 Enhanced RTMP 输出。

音频策略：

- 如果音频已经是 AAC，则 `copy`。
- 如果音频不是 AAC，则转码为 AAC。
- 默认音频码率由 `AUDIO_BITRATE` 控制，默认 `128k`。

注意：HEVC over RTMP 需要播放器支持 Enhanced RTMP。老播放器可能无法播放 HEVC，即使 SRS 和 FFmpeg 都已经正常工作。

## 端口

- `1935`: RTMP
- `1985`: SRS HTTP API
- `8080`: SRS HTTP Server
- `18090`: puller hook/status 服务

## 已验证地址

下面的 `/udp` 地址已经验证可播放，并且断开后会自动释放 FFmpeg：

```text
rtmp://192.168.10.20:1935/udp/239_254_97_96_8550
```

HEVC 示例：

```text
rtmp://192.168.10.20:1935/rtp/239_69_1_27_9802
```

这条能进入 SRS Enhanced RTMP，并能识别为 HEVC Main10 4K；如果源里出现 TS/AAC corrupt，FFmpeg 可能会退出，需要进一步做音频 copy 失败后自动降级转码。
