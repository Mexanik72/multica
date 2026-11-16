import { Mattermost, WeChat } from "@multica/ui/icons";
import { DingTalkMark } from "./dingtalk-mark";
import { LarkMark } from "./lark-mark";
import type { ChannelIntegration } from "./settings-navigation";
import { SlackMark } from "./slack-mark";
import { TelegramMark } from "./telegram-mark";
import { WecomMark } from "./wecom-mark";

export function IntegrationChannelIcon({ channel }: { channel: ChannelIntegration }) {
  const icon = {
    lark: <LarkMark className="h-4 w-4" />,
    slack: <SlackMark className="h-4 w-4" />,
    dingtalk: <DingTalkMark className="h-5 w-5" />,
    wecom: <WecomMark className="h-4 w-4" />,
    weixin: <WeChat className="h-4 w-4" />,
    telegram: <TelegramMark className="h-4 w-4" />,
    mattermost: <Mattermost className="h-4 w-4" />,
  }[channel];

  return (
    <span
      aria-hidden="true"
      data-testid={`integration-channel-icon-${channel}`}
      className="flex size-5 shrink-0 items-center justify-center text-muted-foreground"
    >
      {icon}
    </span>
  );
}
