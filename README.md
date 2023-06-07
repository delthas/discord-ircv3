# discord-ircv3

An IRC <-> Discord bridge with support for modern IRCv3 features.

Features:
- Join / Part / Kick / Disconnect
- [Typing notifications](https://ircv3.net/specs/client-tags/typing.html)
- [Message replies support](https://ircv3.net/specs/client-tags/reply.html)
- Image embedding support
- Advanced [Discord->IRC formatting](https://github.com/delthas/discord-formatting) support

## Setup

Requires Go.

To install:
```shell
go install github.com/delthas/discord-ircv3@master
```

- Create a [Discord app](https://discord.com/developers/docs/getting-started)
- In your bot page, enable the "Server Members Intent" and "Message Content Intent" intents, under "Privileged Gateway Intents"
- Get your application OAuth2 Client ID from the application OAuth2 page
- Add the bot to all your servers by opening the following URL:
```
https://discord.com/oauth2/authorize?permissions=274878024704&scope=bot&client_id=<YOUR_APPLICATION_OAUTH2_CLIENT_ID>
```
- Generate a token for the bot from the application Bot page, and save it for the next step

## Usage

Copy and edit [`config.yaml.example`](config.yaml.example) into `config.yaml`:
- The `discordToken` is the Bot token obtained from the previous step
- The Discord channel IDs can be obtained after "Developer Mode" is enabled in your user settings in the "Advanced" page, by right-clicking channels and selecting "Copy Channel ID"

Then,
```shell
discord-irc
```

## Status

Used in a small-scale deployment for 1 year.

## License

MIT
