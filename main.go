package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"github.com/bwmarrin/discordgo"
	formatting "github.com/delthas/discord-formatting"
	"gopkg.in/irc.v3"
	"gopkg.in/yaml.v2"
	"hash/fnv"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	fBold          byte = '\x02'
	fItalics       byte = '\x1D'
	fUnderline     byte = '\x1F'
	fStrikethrough byte = '\x1E'
	fMonospace     byte = '\x11'
	fColor         byte = '\x03'
	fColorHex      byte = '\x04'
	fReverse       byte = '\x16'
	fReset         byte = '\x0F'
)

type Config struct {
	DiscordToken string            `yaml:"discordToken"`
	Server       string            `yaml:"server"`
	Nick         string            `yaml:"nickname"`
	Channels     map[string]string `yaml:"channels"` // Discord ID to IRC name
}

var cfg Config
var debug bool

var logErr = log.New(os.Stderr, "err:", log.LstdFlags)

var ircClientLock sync.Mutex
var ircClient *irc.Client
var ircReady bool

var discord *discordgo.Session

var idIRCDiscord = make(map[string][]string)
var idDiscordIRC = make(map[string][]string)

func main() {
	flag.BoolVar(&debug, "debug", false, "enable debug logging")
	configPath := flag.String("config", "config.yaml", "config path")
	flag.Parse()
	f, err := os.Open(*configPath)
	if err != nil {
		logErr.Fatal(err)
	}
	yd := yaml.NewDecoder(f)
	err = yd.Decode(&cfg)
	f.Close()
	if err != nil {
		logErr.Fatal(err)
	}

	discord, err = discordgo.New("Bot " + cfg.DiscordToken)
	if err != nil {
		logErr.Fatal(err)
	}
	discord.Identify.Intents = discordgo.IntentsAllWithoutPrivileged | discordgo.IntentsGuildMembers | discordgo.IntentMessageContent
	discord.AddHandler(discordReady)
	discord.AddHandler(discordMessage)
	discord.AddHandler(discordDelete)
	discord.AddHandler(discordReact)
	discord.AddHandler(discordTyping)

	go func() {
		for {
			err = discord.Open()
			if err == nil {
				return
			}
			logErr.Printf("failed opening discord: %v", err)
			time.Sleep(15 * time.Second)
		}
	}()

	go func() {
		for {
			err := ircLoop()
			ircClientLock.Lock()
			ircClient = nil
			ircClientLock.Unlock()
			logErr.Printf("irc error: %v", err)
			time.Sleep(15 * time.Second)
		}
	}()

	select {}
}

func ircLoop() error {
	ircReady = false
	tc, err := tls.Dial("tcp", cfg.Server, nil)
	if err != nil {
		return err
	}
	c := irc.NewClient(tc, irc.ClientConfig{
		Nick:          cfg.Nick,
		User:          "discordircv3",
		Name:          "discord-ircv3 bridge",
		PingFrequency: 10 * time.Minute,
		PingTimeout:   30 * time.Second,
		SendLimit:     500 * time.Millisecond,
		SendBurst:     10,
		Handler:       irc.HandlerFunc(ircHandler),
	})
	c.CapRequest("message-tags", false)
	c.CapRequest("echo-message", false)
	c.CapRequest("draft/message-redaction", false)
	if debug {
		c.Writer.DebugCallback = func(line string) {
			fmt.Printf(">>> %s\n", line)
		}
		c.Reader.DebugCallback = func(line string) {
			fmt.Printf("<<< %s\n", line)
		}
	}
	return c.Run()
}

func ircWrite(m *irc.Message) {
	ircClientLock.Lock()
	defer ircClientLock.Unlock()
	if ircClient == nil {
		return
	}
	if m.Command == "REDACT" && !ircClient.CapEnabled("draft/message-redaction") {
		return
	}
	ircClient.WriteMessage(m)
}

func discordChannel(irc string) string {
	for dc, ic := range cfg.Channels {
		if ic == irc {
			return dc
		}
	}
	return ""
}

type ircStyle struct {
	italics       bool
	bold          bool
	underline     bool
	strikethrough bool
}

func isDigit(s string, i int) bool {
	if i >= len(s) {
		return false
	}
	c := s[i]
	return c >= '0' && c <= '9'
}

var patternMediaLink = regexp.MustCompile("^https?://[^\\s\\x01-\\x16]+\\.(?:jpg|jpeg|png|gif|mp4|webm)$")
var patternURL = regexp.MustCompile("^(https?://[^\\s<]+[^<.,:;\"')\\]\\s])")

func discordFormat(msg string) string {
	msg += string([]byte{fReset})

	var prevStyle ircStyle
	var nextStyle ircStyle
	raw := false
	urlEnd := 0
	var sb strings.Builder
	for i := 0; i < len(msg); i++ {
		c := msg[i]
		if raw && c != '`' {
			sb.WriteByte(c)
			continue
		}
		if i >= urlEnd {
			if loc := patternURL.FindStringIndex(msg[i:]); loc != nil {
				urlEnd = i + loc[1]
			}
		}
		var write string
		switch c {
		case fBold:
			nextStyle.bold = true
		case fItalics:
			nextStyle.italics = true
		case fUnderline:
			nextStyle.underline = true
		case fStrikethrough:
			nextStyle.strikethrough = true
		case fReset:
			nextStyle = ircStyle{}
		case fMonospace, fReverse:
			continue
		case fColor:
			if !isDigit(msg, i+1) {
				continue
			}
			i++
			if isDigit(msg, i+1) {
				i++
			}
			if isDigit(msg, i+2) && msg[i+1] == ',' {
				i += 2
				if isDigit(msg, i+1) {
					i++
				}
			}
			continue
		case fColorHex:
			i += 6
			continue
		case '`':
			if !raw {
				if strings.IndexByte(msg[i+1:], '`') > 0 {
					raw = true
				}
			} else {
				raw = false
			}
			write = string([]byte{c})
		case '\\', '*', '_', '~':
			if i >= urlEnd {
				write = "\\" + string([]byte{c})
			} else {
				// in URL: don't escape chars
				write = string([]byte{c})
			}
		default:
			write = string([]byte{c})
		}
		if write == "" && i+1 < len(msg) {
			continue
		}
		if prevStyle == nextStyle {
			sb.WriteString(write)
			continue
		}
		if prevStyle.italics {
			sb.WriteString("*")
		}
		if prevStyle.bold {
			sb.WriteString("**")
		}
		if prevStyle.underline {
			sb.WriteString("__")
		}
		if prevStyle.strikethrough {
			sb.WriteString("~~")
		}
		prevStyle = ircStyle{}
		if write == "" {
			continue
		}
		sb.WriteString("\u200B")
		if nextStyle.strikethrough {
			sb.WriteString("~~")
		}
		if nextStyle.underline {
			sb.WriteString("__")
		}
		if nextStyle.bold {
			sb.WriteString("**")
		}
		if nextStyle.italics {
			sb.WriteString("*")
		}
		sb.WriteString(write)
		prevStyle = nextStyle
	}
	return sb.String()
}

var patternMention = regexp.MustCompile("^@([^\\s#*_~`]+)#(\\d+)")
var patternEmoji = regexp.MustCompile(":(\\w+):")

func discordTransformPart(channel string, msg string) string {
	discord.State.RLock()
	defer discord.State.RUnlock()
	c, err := discord.State.Channel(channel)
	if err != nil {
		return msg
	}
	g, err := discord.State.Guild(c.GuildID)
	if err != nil {
		return msg
	}
	msg = regexReplaceAll(patternMention, msg, func(groups []int) string {
		original := msg[groups[0]:groups[1]]
		mention := strings.ToLower(msg[groups[2]:groups[3]])
		id := msg[groups[4]:groups[5]]
		if id != "" {
			for _, u := range g.Members {
				if mention == strings.ToLower(u.User.Username) && id == u.User.Discriminator {
					return u.Mention()
				}
			}
			return original
		}
		for _, u := range g.Members {
			if mention == strings.ToLower(u.Nick) {
				return u.Mention()
			}
		}
		for _, u := range g.Members {
			if mention == strings.ToLower(u.User.Username) {
				return u.Mention()
			}
		}
		for _, r := range g.Roles {
			if r.Mentionable && mention == strings.ToLower(r.Name) {
				return r.Mention()
			}
		}
		return original
	})
	msg = regexReplaceAll(patternEmoji, msg, func(groups []int) string {
		original := msg[groups[0]:groups[1]]
		emoji := strings.ToLower(msg[groups[2]:groups[3]])
		for _, e := range g.Emojis {
			if e.Available && emoji == strings.ToLower(e.Name) {
				return e.MessageFormat()
			}
		}
		return original
	})
	return msg
}

func discordTransform(channel, msg string) string {
	var sb strings.Builder
	for len(msg) > 0 {
		rawStart := strings.IndexByte(msg, '`')
		if rawStart >= 0 {
			rawEnd := rawStart + 1 + strings.IndexByte(msg[rawStart+1:], '`')
			if rawEnd >= 0 {
				if rawStart > 0 {
					sb.WriteString(discordTransformPart(channel, msg[:rawStart]))
					sb.WriteString(msg[rawStart : rawEnd+1])
					msg = msg[rawEnd+1:]
					continue
				}
			}
		}
		sb.WriteString(discordTransformPart(channel, msg))
		break
	}
	return sb.String()
}

func discordSend(id string, channel string, msg string, replyID string) {
	msg = discordFormat(msg)
	msg = discordTransform(channel, msg)

	dm := &discordgo.MessageSend{
		Content: msg,
	}
	if replyID != "" {
		dm.Reference = &discordgo.MessageReference{
			MessageID: replyID,
			ChannelID: channel,
		}
	}
	m, err := discord.ChannelMessageSendComplex(channel, dm)
	if err == nil && id != "" {
		idIRCDiscord[id] = append(idIRCDiscord[id], m.ID)
		idDiscordIRC[m.ID] = append(idIRCDiscord[m.ID], id)
	}
}

func ircHandler(c *irc.Client, m *irc.Message) {
	if m.Name == c.CurrentNick() && m.Command != "PRIVMSG" {
		return
	}
	msgID := string(m.Tags["msgid"])
	var replyID string
	if ids := idIRCDiscord[string(m.Tags["+draft/reply"])]; len(ids) > 0 {
		replyID = ids[len(ids)-1]
	}
	handled := true
	switch m.Command {
	case "001":
		for _, ic := range cfg.Channels {
			c.WriteMessage(&irc.Message{
				Command: "JOIN",
				Params:  []string{ic},
			})
		}
		ircClientLock.Lock()
		ircClient = c
		ircClientLock.Unlock()
	case "005":
		if len(m.Params) > 2 {
			for _, param := range m.Params[1 : len(m.Params)-1] {
				key, value, _ := strings.Cut(param, "=")
				switch key {
				case "BOT":
					c.WriteMessage(&irc.Message{
						Command: "MODE",
						Params:  []string{c.CurrentNick(), "+" + value},
					})
				}
			}
		}
		c.WriteMessage(&irc.Message{
			Command: "PING",
			Params:  []string{"ready"},
		})
	case "PONG":
		if m.Params[len(m.Params)-1] == "ready" {
			ircReady = true
		}
	default:
		handled = false
	}
	if handled || !ircReady {
		return
	}
	switch m.Command {
	case "NICK":
		for dc := range cfg.Channels {
			discordSend(msgID, dc, fmt.Sprintf("%c%s%c is now known as %s", fItalics, m.Prefix.Name, fReset, m.Params[0]), replyID)
		}
	case "JOIN":
		dc := discordChannel(m.Params[0])
		if dc == "" {
			return
		}
		discordSend(msgID, dc, fmt.Sprintf("%c%s%c has joined the channel", fItalics, m.Prefix.Name, fReset), replyID)
	case "PART":
		dc := discordChannel(m.Params[0])
		if dc == "" {
			return
		}
		if len(m.Params) > 1 {
			discordSend(msgID, dc, fmt.Sprintf("%c%s%c has left the channel: %s", fItalics, m.Prefix.Name, fReset, m.Params[1]), replyID)
		} else {
			discordSend(msgID, dc, fmt.Sprintf("%c%s%c has left the channel", fItalics, m.Prefix.Name, fReset), replyID)
		}
	case "KICK":
		dc := discordChannel(m.Params[0])
		if dc == "" {
			return
		}
		if len(m.Params) > 2 {
			discordSend(msgID, dc, fmt.Sprintf("%c%s%c was kicked off the channel by %s: %s", fItalics, m.Params[1], fReset, m.Prefix.Name, m.Params[2]), replyID)
		} else {
			discordSend(msgID, dc, fmt.Sprintf("%c%s%c was kicked off the channel by %s", fItalics, m.Params[1], fReset, m.Prefix.Name), replyID)
		}
	case "QUIT":
		for dc := range cfg.Channels {
			if len(m.Params) > 0 {
				discordSend(msgID, dc, fmt.Sprintf("%c%s%c has quit: %s", fItalics, m.Prefix.Name, fReset, m.Params[0]), replyID)
			} else {
				discordSend(msgID, dc, fmt.Sprintf("%c%s%c has quit", fItalics, m.Prefix.Name, fReset), replyID)
			}
		}
	case "REDACT":
		dc := discordChannel(m.Params[0])
		if dc == "" {
			return
		}
		ids := idIRCDiscord[m.Params[1]]
		for _, id := range ids {
			discord.ChannelMessageDelete(dc, id)
		}
	case "TAGMSG":
		dc := discordChannel(m.Params[0])
		if dc == "" {
			return
		}
		if string(m.Tags["+typing"]) == "active" {
			discord.ChannelTyping(dc)
		}
	case "PRIVMSG":
		dc := discordChannel(m.Params[0])
		if dc == "" {
			return
		}
		if m.Name == c.CurrentNick() {
			if discordID := string(m.Tags["+discord"]); discordID != "" {
				idIRCDiscord[msgID] = append(idIRCDiscord[msgID], discordID)
				idDiscordIRC[discordID] = append(idDiscordIRC[discordID], msgID)
			}
			return
		}
		body := m.Params[1]
		if replyID != "" {
			body = strings.TrimPrefix(body, fmt.Sprintf("%s: ", c.CurrentNick()))
		}
		if body[0] == '\x01' {
			body = strings.Trim(body[1:], "\x01")
			verb, data, _ := strings.Cut(body, " ")
			if verb != "ACTION" {
				// drop unknown CTCP
				return
			}
			// a CTCP ACTION is sent as an italicized message
			body = fmt.Sprintf("%c%s", fItalics, data)
		}
		if !strings.ContainsRune(body, ' ') && patternMediaLink.MatchString(body) {
			// send image link in its own message so that it can be embedded by discord
			discordSend("", dc, fmt.Sprintf("%c<%s>", fBold, m.Prefix.Name), replyID)
			discordSend(msgID, dc, body, replyID)
		} else {
			discordSend(msgID, dc, fmt.Sprintf("%c<%s>%c %s", fBold, m.Prefix.Name, fReset, body), replyID)
		}
	case "NOTICE":
		// intentionally not passed through
	}
}

var replacerNewline = strings.NewReplacer(
	"\r\n", " ",
	"\n", " ",
	"\r", " ",
)

var validColors = []int{2, 3, 4, 6, 7, 8, 9, 10, 11, 12, 13}

var discordParser = formatting.NewParser(nil)

func discordIRCFormat(s *discordgo.Session, guildID string, m string) string {
	ast := discordParser.Parse(m)
	var sb strings.Builder
	formatting.Walk(ast, func(nn formatting.Node, entering bool) {
		switch n := nn.(type) {
		case *formatting.TextNode:
			if entering {
				sb.WriteString(n.Content)
			}
		case *formatting.BlockQuoteNode:
			if entering {
				sb.WriteString("“")
			} else {
				sb.WriteString("”")
			}
		case *formatting.CodeNode:
			if entering {
				sb.WriteByte(fMonospace)
				sb.WriteString("`")
				if n.Language != "" {
					sb.WriteString(n.Language)
					sb.WriteString(" ")
				}
				sb.WriteString(n.Content)
				sb.WriteString("`")
				sb.WriteByte(fMonospace)
			}
		case *formatting.SpoilerNode:
			if entering {
				sb.WriteByte(fReverse)
				sb.WriteString("||")
			} else {
				sb.WriteString("||")
				sb.WriteByte(fReverse)
			}
		case *formatting.URLNode:
			if entering {
				sb.WriteString(n.URL)
			}
		case *formatting.EmojiNode:
			if entering {
				sb.WriteString(":")
				sb.WriteString(n.Text)
				sb.WriteString(":")
			}
		case *formatting.ChannelMentionNode:
			if entering {
				if channel, err := s.State.Channel(n.ID); err == nil {
					sb.WriteString("#")
					sb.WriteString(channel.Name)
				} else {
					sb.WriteString("#invalid-channel")
				}
			}
		case *formatting.RoleMentionNode:
			if entering {
				if role, err := s.State.Role(guildID, n.ID); err == nil {
					sb.WriteString("@")
					sb.WriteString(role.Name)
				} else {
					sb.WriteString("@invalid-role")
				}
			}
		case *formatting.UserMentionNode:
			if entering {
				if user, err := s.State.Member(guildID, n.ID); err == nil {
					sb.WriteString("@")
					sb.WriteString(user.Nick)
				} else {
					sb.WriteString("@invalid-user")
				}
			}
		case *formatting.SpecialMentionNode:
			if entering {
				sb.WriteString("@")
				sb.WriteString(n.Mention)
			}
		case *formatting.TimestampNode:
			if entering {
				unix, err := strconv.ParseInt(n.Stamp, 10, 64)
				if err != nil {
					sb.WriteString("<invalid-timestamp>")
					break
				}
				t := time.Unix(unix, 0).Local()
				switch n.Format {
				case "t":
					sb.WriteString(t.Format("15:04 MST"))
				case "T":
					sb.WriteString(t.Format("15:04:05 MST"))
				case "d":
					sb.WriteString(t.Format("2006/01/02 MST"))
				case "D":
					sb.WriteString(t.Format("January 02, 2006 MST"))
				case "f":
					sb.WriteString(t.Format("January 02, 2006 at 15:04 MST"))
				case "F":
					sb.WriteString(t.Format("Monday, January 02, 2006 at 15:04 MST"))
				case "R":
					d := time.Now().Sub(t)
					if d > 0 {
						sb.WriteString(d.String())
						sb.WriteString(" ago")
					} else {
						sb.WriteString("in ")
						sb.WriteString(d.String())
					}
				default:
					sb.WriteString("<invalid-timestamp>")
				}
			}
		case *formatting.BoldNode:
			sb.WriteByte(fBold)
		case *formatting.UnderlineNode:
			sb.WriteByte(fUnderline)
		case *formatting.ItalicsNode:
			sb.WriteByte(fItalics)
		case *formatting.StrikethroughNode:
			sb.WriteByte(fStrikethrough)
		}
	})
	return sb.String()
}

func discordReady(s *discordgo.Session, m *discordgo.Ready) {
	for _, g := range s.State.Guilds {
		s.RequestGuildMembers(g.ID, "", 0, "", false)
	}
}

func discordMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.ID == s.State.User.ID {
		return
	}
	ic, ok := cfg.Channels[m.ChannelID]
	if !ok {
		return
	}
	replyID := ""
	if m.MessageReference != nil {
		if ids := idDiscordIRC[m.MessageReference.MessageID]; len(ids) > 0 {
			replyID = ids[0]
		}
	}

	colorCode := discord.State.MessageColor(m.Message)
	if colorCode == 0 {
		colorCode = m.Author.AccentColor
	}
	var color string
	if colorCode != 0 {
		color = fmt.Sprintf("%c%06X", fColorHex, colorCode)
	} else {
		h := fnv.New32()
		_, _ = h.Write([]byte(m.Author.Username))
		colorCode := validColors[int(h.Sum32())%len(validColors)]
		color = fmt.Sprintf("%c%02d", fColor, colorCode)
	}
	nick := m.Member.Nick
	if nick == "" {
		nick = m.Author.Username
	}
	if len(nick) > 1 {
		r, size := utf8.DecodeRuneInString(nick)
		nick = string([]rune{r, '\u200B'}) + nick[size:]
	}
	prefix := fmt.Sprintf("<%s%s%c> ", color, nick, fReset)

	if len(m.Content) > 0 {
		body := discordIRCFormat(s, m.GuildID, m.Content)
		body = replacerNewline.Replace(body)

		ircWrite(&irc.Message{
			Tags: irc.Tags{
				"+discord":     irc.TagValue(m.ID),
				"+draft/reply": irc.TagValue(replyID),
			},
			Command: "PRIVMSG",
			Params:  []string{ic, prefix + body},
		})
	}
	for _, attachment := range m.Attachments {
		ircWrite(&irc.Message{
			Tags: irc.Tags{
				"+discord":     irc.TagValue(m.ID),
				"+draft/reply": irc.TagValue(replyID),
			},
			Command: "PRIVMSG",
			Params:  []string{ic, prefix + attachment.URL},
		})
	}
}

func discordDelete(s *discordgo.Session, m *discordgo.MessageDelete) {
	// Discord seems to omit the Author in message deletion notifications
	if m.Author != nil && m.Author.ID == s.State.User.ID {
		return
	}
	ic, ok := cfg.Channels[m.ChannelID]
	if !ok {
		return
	}

	for _, id := range idDiscordIRC[m.ID] {
		ircWrite(&irc.Message{
			Command: "REDACT",
			Params:  []string{ic, id},
		})
	}
}

func discordReact(s *discordgo.Session, m *discordgo.MessageReactionAdd) {
	if m.UserID == s.State.User.ID {
		return
	}
	ic, ok := cfg.Channels[m.ChannelID]
	if !ok {
		return
	}
	reaction := m.Emoji.Name
	if reaction == "" {
		return
	}
	replyID := ""
	if ids := idDiscordIRC[m.MessageID]; len(ids) > 0 {
		replyID = ids[0]
	} else {
		return
	}
	ircWrite(&irc.Message{
		Tags: irc.Tags{
			"+draft/react": irc.TagValue(reaction),
			"+draft/reply": irc.TagValue(replyID),
		},
		Command: "TAGMSG",
		Params:  []string{ic},
	})
}

func discordTyping(s *discordgo.Session, m *discordgo.TypingStart) {
	if m.UserID == s.State.User.ID {
		return
	}
	ic, ok := cfg.Channels[m.ChannelID]
	if !ok {
		return
	}
	ircWrite(&irc.Message{
		Tags: irc.Tags{
			"+typing": "active",
		},
		Command: "TAGMSG",
		Params:  []string{ic},
	})
}

func regexReplaceAll(r *regexp.Regexp, s string, f func(s []int) string) string {
	matches := r.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		return s
	}
	var sb strings.Builder
	sb.WriteString(s[:matches[0][0]])
	for i, match := range matches {
		sb.WriteString(f(match))
		if i+1 < len(matches) {
			sb.WriteString(s[matches[i][1]:matches[i+1][0]])
		} else {
			sb.WriteString(s[matches[i][1]:])
		}
	}
	return sb.String()
}
