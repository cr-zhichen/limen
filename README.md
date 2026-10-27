# Limen

Limen 是供 AI 助手和终端用户通过 JumpServer 执行远端命令的 CLI。它支持只读、读写和无限制三档权限，可以在一个持续 SSH 连接中逐条审核并执行命令。需要已有的 JumpServer Access Key，以及提供 Bash 和 flock（util-linux）的 Linux 服务器；只读和读写模式还需要配置 Jev 或普通 LLM。

## 快速安装

macOS / Linux（Intel、AMD 或 ARM64）：

```sh
curl -fsSL https://github.com/cr-zhichen/limen/releases/latest/download/install.sh | sh
```

Windows PowerShell（x64 或 ARM64）：

```powershell
irm https://github.com/cr-zhichen/limen/releases/latest/download/install.ps1 | iex
```

安装后按提示将安装目录加入 PATH，然后运行 `limen version`。

## AI 辅助安装

将下面的提示词交给你的 AI 助手：

```text
请按 https://github.com/cr-zhichen/limen 的 README 为当前电脑安装 Limen，验证 limen version，并把 Limen skill 安装到你使用的 skills 目录。随后运行 limen skill 读取说明，检查已有配置；缺少配置时，询问我 JumpServer 地址、SSH 入口、Access Key 来源、允许的权限和审核模型，按我的选择完成配置。密钥可以引用环境变量、Harmonia 或 1Password，不要在对话中展示密钥，不覆盖已有配置，也不要自行启用无限制权限。完成后查询可访问的资产，不执行服务器修改命令。
```

## 配置

```sh
limen config init
```

按生成文件中的注释填写。全部配置项、作用、默认值和凭据来源见 [配置示例](config.example.toml)，也可以运行 `limen config example` 查看。

```sh
limen config check
```

其他配置文件通过 `--config <路径>` 指定。

## 连接服务器

```sh
limen assets
limen accounts <资产ID>
limen host-key
```

向管理员核实主机指纹，再保存：

```sh
limen host-key --trust '<已核实的SHA256指纹>'
limen connect --asset '<资产ID>' --account '<账号alias>' --permission read-only
```

连接后的命令提交和正常关闭方式见 `limen skill`。单条命令也可以使用 `limen exec --help`。

## AI skill

```sh
limen skill install
```

默认安装到 `~/.agents/skills/limen`。其他 AI 工具使用 `limen skill install --dir <skills根目录>`。

## 权限

| 权限 | 允许范围 |
| --- | --- |
| `read-only` | 读取账号有权访问的信息；允许修改、删除配置的可写目录内的文件 |
| `read-write` | 正常文件与数据库读写、普通删除、配置和权限修改、服务管理；禁止大范围毁灭性操作 |
| `unrestricted` | 跳过命令审核，仍受 JumpServer 和远端账号权限限制 |

毁灭性操作包括清空系统关键目录或主要业务数据、覆盖整块磁盘、破坏分区表、清空整个业务数据库等。常规服务重启、普通文件删除不会仅因为有影响而归入此类。

## 更新

```sh
limen update --check
limen update
```

正式版在查询资产、账号和连接服务器时自动检查更新，每天最多一次，只提醒、不自动安装。设置环境变量 `LIMEN_NO_UPDATE_CHECK=1` 可以关闭自动检查，手动检查仍可使用。

## 注意事项

- 正常退出会清空配置的可写目录，最终结果应提前保存。
- 命令审核依赖模型判断，不替代服务器账号权限隔离；配置及凭据应由可信用户管理。

## 许可证

[MIT](LICENSE)
