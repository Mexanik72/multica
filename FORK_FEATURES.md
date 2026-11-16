# Доработки форка Multica

Здесь перечислены доработки ветки `Mexanik72/multica:fork/main` относительно `upstream/main`. Обновляйте этот список при добавлении, изменении или удалении доработки. Порядок синхронизации описан в [FORK_WORKFLOW.md](FORK_WORKFLOW.md).

| Доработка | Что добавлено | Документация и код |
| --- | --- | --- |
| Интеграция Mattermost | Бот принимает личные сообщения и упоминания в каналах, отвечает в тредах и создаёт задачи по команде `/issue`. В self-hosted установках Mattermost доступен вместе с WeChat. | [Настройка и команды](apps/docs/content/docs/mattermost-bot-integration.mdx) |
| Уведомления об упоминаниях в Mattermost | Общий бот отправляет личное сообщение при прямом упоминании пользователя в задаче или комментарии. Получатель определяется по имени пользователя Mattermost. | [Уведомления](server/internal/integrations/mattermost/mention_notifications.go) |
| Доступ Hermes к инструментам MCP | Доступ к инструментам рабочего пространства можно настроить по группам для агента Hermes на назначенном runtime. | [Хранение и применение политики](server/internal/daemon/execenv/hermes_home.go) |
| Сохранение и просмотр навыков Hermes | Навыки, созданные во время задач, сохраняются на daemon для агента и профиля. Эти навыки доступны для просмотра, пока runtime онлайн. | [Хранение навыков](server/internal/daemon/execenv/hermes_skills.go) |
| Docker-образы форка | CI проверяет self-hosted сборку и публикует образы backend и веб-клиента в GHCR с общим тегом коммита. | [Развёртывание](FORK_WORKFLOW.md#ci-для-docker), [workflow](.github/workflows/fork-docker.yml) |
| Руководство по достижению цели с wakeups | Пример настройки задачи, в которой агент продолжает работу по напоминаниям до достижения цели. | [Руководство](apps/docs/content/docs/reach-a-goal-with-wakeups.mdx) |
