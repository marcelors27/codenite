# codenite worker (Todoist -> Codex -> GitHub PR)

Worker agent em Go que:

- lê tasks do Todoist com label configurada (ex: `ia:do`)
- mapeia cada lista/projeto do Todoist para um repositório GitHub
- baixa/atualiza o repositório
- executa um comando de IA (Codex inicialmente) para implementar a task
- commit/push e abre PR automaticamente
- adiciona label `Coding` ao iniciar e troca para `PR Opened` ao finalizar
- comenta e fecha a task no Todoist (opcional)
- evita reprocessamento no polling (ignora tasks com `Coding`/`PR Opened` e não processa o mesmo `task.ID` duas vezes no mesmo processo)
- salva a saída da IA (`stdout`/`stderr`) em comentários da task (com chunking quando necessário)

## Requisitos

- Go 1.26+
- `git` no PATH
- `codex` CLI (ou outro comando compatível) no PATH
- token do Todoist
- token do GitHub com permissão de `repo`

## Configuração

Crie `config.json`:

```json
{
  "worker": {
    "poll_interval_seconds": 60,
    "work_root": "/tmp/codenite-work",
    "dry_run": false,
    "close_task_on_pr": true,
    "comment_on_task": true
  },
  "task_source": {
    "provider": "todoist",
    "todoist": {
      "token": "TODOIST_TOKEN",
      "label": "ia:do"
    }
  },
  "ai": {
    "provider": "codex",
    "command": "codex exec --full-auto \"$TASK_PROMPT\"",
    "env": {
      "OPENAI_API_KEY": "${OPENAI_API_KEY}"
    }
  },
  "vcs": {
    "provider": "github",
    "github": {
      "token": "GITHUB_TOKEN",
      "draft": true
    }
  },
  "repositories": {
    "123456789": {
      "repo": "marcelors27/chroma-monorepo",
      "base_branch": "main"
    },
    "987654321": {
      "repo": "marcelors27/chroma-monorepo",
      "base_branch": "develop"
    }
  }
}
```

`repositories` mapeia `project_id` do Todoist para `owner/repo` do GitHub.

## Rodar

```bash
go run ./cmd/worker -config ./config.json -once
```

Loop contínuo:

```bash
go run ./cmd/worker -config ./config.json
```

## Variáveis disponíveis no comando de IA

- `REPO_PATH`
- `REPO` (formato `owner/repo`)
- `TASK_ID`
- `TASK_TITLE`
- `TASK_DESCRIPTION`
- `TASK_PROMPT`

## Autenticação da IA no Railway

- Configure a variável de ambiente `OPENAI_API_KEY` no serviço do Railway.
- Em `ai.env`, mapeie o nome esperado pelo CLI para `${OPENAI_API_KEY}`.
- O worker expande `${...}` usando o ambiente do processo no `config.json` (incluindo tokens) e injeta no comando da IA.

Isso facilita trocar o provedor de IA depois sem mexer no core do worker.

## Deploy no Railway

- O repositório já contém `railway.json` usando build por `Dockerfile`.
- Configure as env vars no serviço Railway:
- `OPENAI_API_KEY`
- `GITHUB_TOKEN`
- `TODOIST_TOKEN`
- `WORKER_CONFIG_JSON` (JSON do config em uma linha)

Exemplo de `WORKER_CONFIG_JSON`:

```json
{"worker":{"poll_interval_seconds":60,"work_root":"/tmp/codenite-work","dry_run":false,"close_task_on_pr":true,"comment_on_task":true},"task_source":{"provider":"todoist","todoist":{"token":"${TODOIST_TOKEN}","label":"ia:do"}},"ai":{"provider":"codex","command":"codex exec --full-auto \"$TASK_PROMPT\"","env":{"OPENAI_API_KEY":"${OPENAI_API_KEY}"}},"vcs":{"provider":"github","github":{"token":"${GITHUB_TOKEN}","draft":true}},"repositories":{"123456789":{"repo":"marcelors27/chroma-monorepo","base_branch":"main"}}}
```

Arquivo auxiliar com exemplo de env:
- `.env.railway.example`

## Extensões futuras

- novas fontes de task implementando `TaskSource`
- novos provedores de IA implementando `AIProvider`
- novos provedores de VCS implementando `VCSProvider`
