# PoolMate

你自己的 WorkBuddy / CodeBuddy 账号池网关：把多个账号聚合成一个池，对外提供 OpenAI / Anthropic 双协议 API，内置 Web 管理面板。Go 纯标准库实现，零第三方依赖，单文件可执行程序。

## 快速开始

1. 双击 `start.bat`（或命令行运行 `poolmate.exe`）
2. **首次运行务必在 iOA 中放行**：iOA 主界面 → 安全 → 软件访问详情 → 把 `poolmate.exe` 设为允许访问网络（办公网零信任要求，未放行时所有上游请求会被拦截为 403）
3. 浏览器打开 `http://127.0.0.1:7864/`，输入管理密钥（首次启动打印在控制台，同时写入 `config.json` 的 `admin_key`，形如 `pm-adm-xxxx`）
4. 面板里点「添加账号」，浏览器完成官方授权登录即自动入库；也可以直接把凭证 JSON（兼容 PoolService / wb2api 的 `auths/workbuddy-*.json`，含带 BOM 的文件）放进 `auths/` 目录
5. 在「API 密钥」页创建下游密钥，客户端指向 `http://127.0.0.1:7864/v1` 即可使用

环境变量可覆盖配置：`POOLMATE_LISTEN`、`POOLMATE_DATA_DIR`、`POOLMATE_ADMIN_KEY`。

## 客户端接入

OpenAI 兼容（Claude Code、Cline、Cherry Studio 等通用客户端）：

```
Base URL: http://127.0.0.1:7864/v1
API Key:  sk-pm-xxxx（面板里创建的密钥）
```

Anthropic Messages（Claude Code 原生协议）：

```
export ANTHROPIC_BASE_URL=http://127.0.0.1:7864
export ANTHROPIC_AUTH_TOKEN=sk-pm-xxxx
```

端点：`POST /v1/chat/completions`、`POST /v1/messages`、`POST /v1/messages/count_tokens`、`GET /v1/models`、`GET /healthz`。

## 核心机制

- 选号：余额比例 ×10 + 临期浪费加速 ×8 + 闲置补偿（0.5/h 封顶 5）三因子打分，Top-5 短名单等权打乱后加权抽签；100ms 防惊群；会话粘性（30 分钟滚动）
- 冷却：429 软冷却 600s 指数退避封顶 2h；402 硬冷却到次日 04:00（余额恢复自动解冻）；404 浅冷却 5 分钟；6004 模型级独立冷却；连败 3 次熔断 30m 指数退避封顶 6h
- 成本分层：免费 > 未知 > 收费；免费号垄断且 30 分钟未探索时自动搭车探索未知号
- 协议转换：Anthropic Messages 完整转 OpenAI 格式（system/工具/图片/thinking），流式 SSE 状态机回译
- 凭证：与 PoolService 的 auths/*.json 完全同构，可直接混用；token 临期自动刷新，CAS 写回防并发覆盖

## 相比 PoolService 的新增功能

1. 告警通知中心：企业微信 / Bark / Server酱 / Telegram / 自定义 Webhook 五通道，去重防轰炸，面板可测试与回看
2. 凭证保险箱：AES-256-GCM + PBKDF2(210k) 加密落盘，主密码 + 机器指纹绑定，换机无法解密
3. API 调试台：面板内置 Playground，选模型直接对话，SSE 实时渲染，验证池子链路
4. 凭证健康检查：定时实测刷新每个账号 token，提前发现失效应激，避免请求时才暴雷
5. 用量热力图：7×24 请求分布，一眼看出高峰时段，辅助调整签到/保活计划
6. 账号级日预算：每个账号可设每日积分上限，防止单号被打爆触发风控
7. 一键诊断：配置、目录可写、账号池、凭证到期、上游连通、模型目录、时钟偏移、监听端口八项自检
8. 自动备份：每天打包 auths + state.json，滚动保留 N 份；面板可手动导出加密备份包（跨机迁移用）
9. 模型映射与隐藏：别名映射（如 `claude-sonnet=glm-5.3`，客户端请求别名自动路由到上游真实模型，响应仍回显别名）+ 模型隐藏（支持尾部 `*` 通配，不出现在 /v1/models）；设置页可视化配置
10. 模型与档位页：完整展示上游模型元数据（积分倍率、默认档、支持的思考档位、思考是否可关、上下文长度档、工具/视觉能力、描述）；每模型可覆盖默认思考档位（注入 reasoning_effort）、上下文长度档（注入 context_window，从上游 supportedLengths 中选择）、最大输出（钳制 max_tokens），客户端显式参数优先，修改即生效

## 目录结构

```
poolmate/
  poolmate.exe        主程序（单文件，前端已内嵌）
  start.bat           一键启动并打开面板
  config.example.json 配置参考（实际配置在 data/config.json）
  auths/              账号凭证（明文 .json 或加密 .json.enc）
  data/               运行数据（config/state/keys/日志/备份）
```

## 安全提示

- 默认只监听 `127.0.0.1`，如需局域网共享请改 `listen` 并务必设置强 `admin_key` 与下游密钥
- `auths/` 与 `data/` 包含敏感凭证，建议开启凭证保险箱加密
- 管理密钥不要通过不可信渠道传输；面板支持 `?key=` 仅供本机临时使用
