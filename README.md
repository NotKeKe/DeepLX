## This is a fork version from [OwO-Network/DLX](https://github.com/OwO-Network/DLX)

### 這個 fork 版本加上了幾個功能
1. Proxy list 的支援
  - 只要在 [compose file](./compose.yaml) 加上底下這個就可以用，應該也支援原本的 `PROXY` 環境變數
    ```yaml
      environment:
        # - PROXY=PROXY_URL
        - PROXY_LIST_URL=PROXY_URL_TXT
        - PROXY_UPDATE_INTERVAL=5
        - PROXY_LATENCY_TIMEOUT=3
        # 選用：每次更新最多探測/保留幾個 proxy，以及同時探測的併發數。
        # 預設 200 / 32；清單很大（例如 8 萬多行）時，這兩個上限決定記憶體用量。
        # - PROXY_MAX_POOL=200
        # - PROXY_PROBE_CONCURRENCY=32
    ```
    - PROXY_URL_TXT 是一個網路連結，如 `https://raw.githubusercontent.com/proxifly/free-proxy-list/main/proxies/all/data.txt`
    - 清單只會取樣其中 `PROXY_MAX_POOL` 條來探測與輪替使用（每次更新會換一個區段），
      探測失敗的 proxy 會標記為 failed 並跳過；同時建議在 compose file 加上
      `GOMEMLIMIT=256MiB`，讓 Go runtime 有記憶體上限。
2. cloudflare warp 的支援
  - 加上了 `caomingjun/warp`，為了隱私
    - 我不希望那些免費 proxy 紀錄我的 IP，所以我加上了 cloudflare warp。
    - 不過我不確定這個功能是否正常

### 網路請求行為
- 預期: **[本地] -> [Cloudflare Warp] -> [設定的 proxy] -> [deepl]**
- 實際
  - 單純使用 `curl` 請求時，一定會經過 `warp`
  - 如果加上 `proxy` 的話，理論上也會經過，但是礙於我的技術有點差，所以我沒辦法確定他是否是 `[warp] -> [proxy] -> [deepl]`
    - 我用抓包看過，看起來應該是有經過 `warp`，所以我就當作這個流程可以跑得通了。

## 警告
### 請勿將其用於企業、商用、任何違法行為。
### 其他授權與原作者相同，請參考以下 README。


---


<!--
 * @Author: Vincent Young
 * @Date: 2022-10-18 07:32:29
 * @LastEditors: Vincent Yang
 * @LastEditTime: 2026-07-08 00:00:00
 * @FilePath: /DLX/README.md
 * @Telegram: https://t.me/missuo
 * 
 * Copyright © 2022 by Vincent, All Rights Reserved. 
-->

# DLX

[![GitHub Workflow][1]](https://github.com/OwO-Network/DLX/actions)
[![Go Version][2]](https://github.com/OwO-Network/DLX/blob/main/go.mod)
[![Go Report][3]](https://goreportcard.com/badge/github.com/OwO-Network/DLX)
[![GitHub License][4]](https://github.com/OwO-Network/DLX/blob/main/LICENSE)
[![Docker Pulls][5]](https://hub.docker.com/r/missuo/deeplx)
[![Releases][6]](https://github.com/OwO-Network/DLX/releases)

[1]: https://img.shields.io/github/actions/workflow/status/OwO-Network/DLX/release.yaml?logo=github
[2]: https://img.shields.io/github/go-mod/go-version/OwO-Network/DLX?logo=go
[3]: https://goreportcard.com/badge/github.com/OwO-Network/DLX
[4]: https://img.shields.io/github/license/OwO-Network/DLX
[5]: https://img.shields.io/docker/pulls/missuo/deeplx?logo=docker
[6]: https://img.shields.io/github/v/release/OwO-Network/DLX?logo=smartthings

> [!IMPORTANT]
> **Disclaimer:** DLX is an independent, open-source project. It is **not** an official DeepL product, and it is **not** affiliated with, endorsed by, or sponsored by DeepL SE in any way. "DeepL" is a registered trademark of DeepL SE. Any reference to DeepL in this repository is made solely to describe interoperability with the DeepL translation service.

## Why was this project renamed?

In July 2026, we received a trademark notice forwarded by GitHub Trust & Safety, submitted on behalf of DeepL SE. The notice stated that this project's former name, "DeepLX", contained the registered trademark "DeepL" and might cause confusion about whether the project is authorized or endorsed by DeepL SE.

It never was, and it never claimed to be. To resolve the matter and remove any possible confusion, we renamed the repository to **DLX** and removed DeepL branding from the project. To state it plainly one more time: **this project is not an official DeepL project and has no relationship with DeepL SE whatsoever.**

DLX is a self-hosted translation API server written in Go. It exposes a simple HTTP API on port `1188`.

## Usage

### Docker

Docker Hub and GHCR image names remain `deeplx` (only the GitHub repository was renamed to DLX):

```bash
docker run -d -p 1188:1188 ghcr.io/owo-network/deeplx:latest
# or: docker run -d -p 1188:1188 missuo/deeplx:latest
```

Or use the provided [`compose.yaml`](compose.yaml):

```bash
docker compose up -d
```

### Binary

Download the binary for your platform from [Releases](https://github.com/OwO-Network/DLX/releases) and run it (artifact names remain `deeplx_*`):

```bash
./deeplx
```

### Translate

`text` accepts either a single string or an array of strings. A string is answered with a string in `data`; an array is answered with a position-aligned array of the same length, and the whole batch travels in a single upstream request.

```bash
# Single text
curl -X POST http://localhost:1188/translate \
  -H "Content-Type: application/json" \
  -d '{"text": "Hello, world!", "source_lang": "EN", "target_lang": "ZH"}'
```

```json
{"alternatives":null,"code":200,"data":"你好，世界！","id":123,"method":"Free","source_lang":"EN","target_lang":"ZH"}
```

```bash
# Batch: one entry per text, answers come back in the same order
curl -X POST http://localhost:1188/translate \
  -H "Content-Type: application/json" \
  -d '{"text": ["Hello, world!", "Good morning"], "source_lang": "EN", "target_lang": "ZH"}'
```

```json
{"alternatives":null,"code":200,"data":["你好，世界！","早上好"],"id":123,"method":"Free","source_lang":"EN","target_lang":"ZH"}
```

Batch limits and failure modes:

- The anonymous limit is **1500 characters for the whole request**, counted in UTF-16 code units — what JavaScript's `String.prototype.length` reports, so a CJK character costs 1 and an astral one (emoji, historic scripts) costs 2 — across every text in the array, not 1500 per text. A batch over that total is rejected with `413` and `text exceeds maximum length: N characters (anonymous oneshot limit is 1500)`, so a large batch has to be chunked by total length rather than by segment count. There is no separate cap on the number of texts.
- A blank text — empty, or nothing but whitespace — is answered with itself and never sent, so a blank line in a segmented document keeps its place instead of failing the request. `"text": []` is answered with `{"data": []}`; a body with no `text` field at all, or `"text": null`, is rejected with `400 {"code":400,"message":"Invalid request payload"}`.
- A batch is all-or-nothing: if DeepL returns a different number of translations or leaves one of them empty, the request fails with `503 {"code":503,"message":"Translation failed"}` instead of answering with placeholders or a shorter, misaligned array.

## Discussion Group
[Telegram Group](https://t.me/+8KDGHKJCxEVkNzll)

## Acknowledgements

### Contributors

<a href="https://github.com/OwO-Network/DLX/graphs/contributors">
  <img src="https://contrib.rocks/image?repo=OwO-Network/DLX&anon=0" />
</a>

## Activity
![Alt](https://repobeats.axiom.co/api/embed/5f473f85db27cb30028a2f3db7a560f3577a4860.svg "Repobeats analytics image")

## License
[MIT](LICENSE)
