## This is a fork version from [OwO-Network/DeepLX](https://github.com/OwO-Network/DeepLX)

### 這個 fork 版本加上了幾個功能
1. Proxy list 的支援
  - 只要在 [compose file](./compose.yaml) 加上底下這個就可以用，應該也支援原本的 `PROXY` 環境變數
    ```yaml
      environment:
        # - PROXY=PROXY_URL
        - PROXY_LIST_URL=PROXY_URL_TXT
        - PROXY_UPDATE_INTERVAL=5
        - PROXY_LATENCY_TIMEOUT=3
    ```
    - PROXY_URL_TXT 是一個網路連結，如 `https://raw.githubusercontent.com/proxifly/free-proxy-list/main/proxies/all/data.txt`
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
[![Docker Pulls][5]](https://hub.docker.com/r/missuo/dlx)
[![Releases][6]](https://github.com/OwO-Network/DLX/releases)

[1]: https://img.shields.io/github/actions/workflow/status/OwO-Network/DLX/release.yaml?logo=github
[2]: https://img.shields.io/github/go-mod/go-version/OwO-Network/DLX?logo=go
[3]: https://goreportcard.com/badge/github.com/OwO-Network/DLX
[4]: https://img.shields.io/github/license/OwO-Network/DLX
[5]: https://img.shields.io/docker/pulls/missuo/dlx?logo=docker
[6]: https://img.shields.io/github/v/release/OwO-Network/DLX?logo=smartthings

> [!IMPORTANT]
> **Disclaimer:** DLX is an independent, open-source project. It is **not** an official DeepL product, and it is **not** affiliated with, endorsed by, or sponsored by DeepL SE in any way. "DeepL" is a registered trademark of DeepL SE. Any reference to DeepL in this repository is made solely to describe interoperability with the DeepL translation service.

## Why was this project renamed?

In July 2026, we received a trademark notice forwarded by GitHub Trust & Safety, submitted on behalf of DeepL SE. The notice stated that this project's former name, "DeepLX", contained the registered trademark "DeepL" and might cause confusion about whether the project is authorized or endorsed by DeepL SE.

It never was, and it never claimed to be. To resolve the matter and remove any possible confusion, we renamed the repository to **DLX** and removed DeepL branding from the project. To state it plainly one more time: **this project is not an official DeepL project and has no relationship with DeepL SE whatsoever.**

DLX is a self-hosted translation API server written in Go. It exposes a simple HTTP API on port `1188`.

## Usage

### Docker

```bash
docker run -d -p 1188:1188 ghcr.io/owo-network/dlx:latest
```

Or use the provided [`compose.yaml`](compose.yaml):

```bash
docker compose up -d
```

### Binary

Download the binary for your platform from [Releases](https://github.com/OwO-Network/DLX/releases) and run it:

```bash
./dlx
```

### Translate

```bash
curl -X POST http://localhost:1188/translate \
  -H "Content-Type: application/json" \
  -d '{"text": "Hello, world!", "source_lang": "EN", "target_lang": "ZH"}'
```

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
