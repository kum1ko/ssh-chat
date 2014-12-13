package main

import (
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/terminal"
)

const MSG_BUFFER int = 10

const HELP_TEXT string = `-> Available commands:
   /about
   /exit
   /help
   /list
   /nick $NAME
   /whois $NAME
`

const ABOUT_TEXT string = `-> ssh-chat is made by @shazow.

   It is a custom ssh server built in Go to serve a chat experience
   instead of a shell.

   Source: https://github.com/shazow/ssh-chat

   For more, visit shazow.net or follow at twitter.com/shazow
`

var autoCompleteFunc func(line string, pos int, key rune) (newLine string, newPos int, ok bool) = nil

type Client struct {
	Server        *Server
	Conn          *ssh.ServerConn
	Msg           chan string
	Name          string
	Color         string
	Op            bool
	ready         chan struct{}
	term          *terminal.Terminal
	termWidth     int
	termHeight    int
	silencedUntil time.Time
}

func NewClient(server *Server, conn *ssh.ServerConn) *Client {
	if autoCompleteFunc == nil {
		autoCompleteFunc = createAutoCompleteFunc(server)
	}
	return &Client{
		Server: server,
		Conn:   conn,
		Name:   conn.User(),
		Color:  RandomColor(),
		Msg:    make(chan string, MSG_BUFFER),
		ready:  make(chan struct{}, 1),
	}
}

func (c *Client) ColoredName() string {
	return ColorString(c.Color, c.Name)
}

func (c *Client) Write(msg string) {
	c.term.Write([]byte(msg + "\r\n"))
}

func (c *Client) WriteLines(msg []string) {
	for _, line := range msg {
		c.Write(line)
	}
}

func (c *Client) IsSilenced() bool {
	return c.silencedUntil.After(time.Now())
}

func (c *Client) Silence(d time.Duration) {
	c.silencedUntil = time.Now().Add(d)
}

func (c *Client) Resize(width int, height int) error {
	err := c.term.SetSize(width, height)
	if err != nil {
		logger.Errorf("Resize failed: %dx%d", width, height)
		return err
	}
	c.termWidth, c.termHeight = width, height
	return nil
}

func (c *Client) Rename(name string) {
	c.Name = name
	c.term.SetPrompt(fmt.Sprintf("[%s] ", c.ColoredName()))
}

func (c *Client) Fingerprint() string {
	return c.Conn.Permissions.Extensions["fingerprint"]
}

func (c *Client) handleShell(channel ssh.Channel) {
	defer channel.Close()

	// FIXME: This shouldn't live here, need to restructure the call chaining.
	c.Server.Add(c)
	go func() {
		// Block until done, then remove.
		c.Conn.Wait()
		c.Server.Remove(c)
	}()

	go func() {
		for msg := range c.Msg {
			c.Write(msg)
		}
	}()

	for {
		line, err := c.term.ReadLine()
		if err != nil {
			break
		}

		parts := strings.SplitN(line, " ", 3)
		isCmd := strings.HasPrefix(parts[0], "/")

		if isCmd {
			// TODO: Factor this out.
			switch parts[0] {
			case "/test-colors": // Shh, this command is a secret!
				c.Write(ColorString("32", "Lorem ipsum dolor sit amet,"))
				c.Write("consectetur " + ColorString("31;1", "adipiscing") + " elit.")
			case "/exit":
				channel.Close()
			case "/help":
				c.WriteLines(strings.Split(HELP_TEXT, "\n"))
			case "/about":
				c.WriteLines(strings.Split(ABOUT_TEXT, "\n"))
			case "/me":
				me := strings.TrimLeft(line, "/me")
				if me == "" {
					me = " is at a loss for words."
				}
				msg := fmt.Sprintf("** %s%s", c.ColoredName(), me)
				if c.IsSilenced() || len(msg) > 1000 {
					c.Msg <- fmt.Sprintf("-> Message rejected.")
				} else {
					c.Server.Broadcast(msg, nil)
				}
			case "/nick":
				if len(parts) == 2 {
					c.Server.Rename(c, parts[1])
				} else {
					c.Msg <- fmt.Sprintf("-> Missing $NAME from: /nick $NAME")
				}
			case "/whois":
				if len(parts) == 2 {
					client := c.Server.Who(parts[1])
					if client != nil {
						version := RE_STRIP_TEXT.ReplaceAllString(string(client.Conn.ClientVersion()), "")
						if len(version) > 100 {
							version = "Evil Jerk with a superlong string"
						}
						c.Msg <- fmt.Sprintf("-> %s is %s via %s", client.ColoredName(), client.Fingerprint(), version)
					} else {
						c.Msg <- fmt.Sprintf("-> No such name: %s", parts[1])
					}
				} else {
					c.Msg <- fmt.Sprintf("-> Missing $NAME from: /whois $NAME")
				}
			case "/list":
				names := c.Server.List(nil)
				c.Msg <- fmt.Sprintf("-> %d connected: %s", len(names), strings.Join(names, ", "))
			case "/ban":
				if !c.Server.IsOp(c) {
					c.Msg <- fmt.Sprintf("-> You're not an admin.")
				} else if len(parts) != 2 {
					c.Msg <- fmt.Sprintf("-> Missing $NAME from: /ban $NAME")
				} else {
					client := c.Server.Who(parts[1])
					if client == nil {
						c.Msg <- fmt.Sprintf("-> No such name: %s", parts[1])
					} else {
						fingerprint := client.Fingerprint()
						client.Write(fmt.Sprintf("-> Banned by %s.", c.ColoredName()))
						c.Server.Ban(fingerprint, nil)
						client.Conn.Close()
						c.Server.Broadcast(fmt.Sprintf("* %s was banned by %s", parts[1], c.ColoredName()), nil)
					}
				}
			case "/op":
				if !c.Server.IsOp(c) {
					c.Msg <- fmt.Sprintf("-> You're not an admin.")
				} else if len(parts) != 2 {
					c.Msg <- fmt.Sprintf("-> Missing $NAME from: /op $NAME")
				} else {
					client := c.Server.Who(parts[1])
					if client == nil {
						c.Msg <- fmt.Sprintf("-> No such name: %s", parts[1])
					} else {
						fingerprint := client.Fingerprint()
						client.Write(fmt.Sprintf("-> Made op by %s.", c.ColoredName()))
						c.Server.Op(fingerprint)
					}
				}
			case "/silence":
				if !c.Server.IsOp(c) {
					c.Msg <- fmt.Sprintf("-> You're not an admin.")
				} else if len(parts) < 2 {
					c.Msg <- fmt.Sprintf("-> Missing $NAME from: /silence $NAME")
				} else {
					duration := time.Duration(5) * time.Minute
					if len(parts) >= 3 {
						parsedDuration, err := time.ParseDuration(parts[2])
						if err == nil {
							duration = parsedDuration
						}
					}
					client := c.Server.Who(parts[1])
					if client == nil {
						c.Msg <- fmt.Sprintf("-> No such name: %s", parts[1])
					} else {
						client.Silence(duration)
						client.Write(fmt.Sprintf("-> Silenced for %s by %s.", duration, c.ColoredName()))
					}
				}
			default:
				c.Msg <- fmt.Sprintf("-> Invalid command: %s", line)
			}
			continue
		}

		msg := fmt.Sprintf("%s: %s", c.ColoredName(), line)
		if c.IsSilenced() || len(msg) > 1000 {
			c.Msg <- fmt.Sprintf("-> Message rejected.")
			continue
		}
		c.Server.Broadcast(msg, c)
	}

}

func (c *Client) handleChannels(channels <-chan ssh.NewChannel) {
	prompt := fmt.Sprintf("[%s] ", c.ColoredName())

	hasShell := false

	for ch := range channels {
		if t := ch.ChannelType(); t != "session" {
			ch.Reject(ssh.UnknownChannelType, fmt.Sprintf("unknown channel type: %s", t))
			continue
		}

		channel, requests, err := ch.Accept()
		if err != nil {
			logger.Errorf("Could not accept channel: %v", err)
			continue
		}
		defer channel.Close()

		c.term = terminal.NewTerminal(channel, prompt)
		c.term.AutoCompleteCallback = autoCompleteFunc

		for req := range requests {
			var width, height int
			var ok bool

			switch req.Type {
			case "shell":
				if c.term != nil && !hasShell {
					go c.handleShell(channel)
					ok = true
					hasShell = true
				}
			case "pty-req":
				width, height, ok = parsePtyRequest(req.Payload)
				if ok {
					err := c.Resize(width, height)
					ok = err == nil
				}
			case "window-change":
				width, height, ok = parseWinchRequest(req.Payload)
				if ok {
					err := c.Resize(width, height)
					ok = err == nil
				}
			}

			if req.WantReply {
				req.Reply(ok, nil)
			}
		}
	}
}

func createAutoCompleteFunc(server *Server) func(string, int, rune) (string, int, bool) {
	return func(line string, pos int, key rune) (newLine string, newPos int, ok bool) {
		if key == 9 {
			shortLine := strings.Split(line[:pos], " ")
			partialNick := shortLine[len(shortLine)-1]

			nicks := server.List(&partialNick)
			if len(nicks) > 0 {
				nick := nicks[len(nicks)-1]
				posPartialNick := pos - len(partialNick)

				newLine = strings.Replace(line[posPartialNick:],
					partialNick, nick, 1)
				newLine = line[:posPartialNick] + newLine
				newPos = pos + (len(nick) - len(partialNick))
				ok = true
				fmt.Println(newLine)
			}
		} else {
			ok = false
		}
		return
	}
}
