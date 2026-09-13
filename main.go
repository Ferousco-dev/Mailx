package main

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"strings"
)

type Session struct {
	saidHello  bool
	sender     string
	recipients []string
}

func main() {
	listener, err := net.Listen("tcp", ":2525")
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("MailX SMTP server running on localhost:2525")

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Println("connection error:", err)
			continue
		}

		go handleConnection(conn)
	}
}

func handleConnection(conn net.Conn) {
	defer conn.Close()

	session := Session{}

	fmt.Println("New SMTP client connected:", conn.RemoteAddr())

	reader := bufio.NewReader(conn)

	// SMTP greeting
	fmt.Fprint(conn, "220 localhost MailX SMTP Server\r\n")

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			fmt.Println("Client disconnected")
			return
		}

		line = strings.TrimSpace(line)

		fmt.Println("CLIENT:", line)

		command := strings.ToUpper(line)

		switch {

		case strings.HasPrefix(command, "EHLO"):
			session.saidHello = true

			fmt.Println("SESSION saidHello:", session.saidHello)

			fmt.Fprint(conn, "250-localhost\r\n")
			fmt.Fprint(conn, "250 OK\r\n")

		case strings.HasPrefix(command, "HELO"):
			session.saidHello = true

			fmt.Println("SESSION saidHello:", session.saidHello)

			fmt.Fprint(conn, "250 localhost\r\n")

		case strings.HasPrefix(command, "MAIL FROM:"):
			if !session.saidHello {
				fmt.Fprint(conn, "503 Bad sequence of commands\r\n")
				continue
			}

			session.sender = strings.TrimSpace(
				line[len("MAIL FROM:"):],
			)

			fmt.Println("SESSION sender:", session.sender)

			fmt.Fprint(conn, "250 OK\r\n")

		case strings.HasPrefix(command, "RCPT TO:"):
			fmt.Println("CURRENT SENDER:", session.sender)

			if session.sender == "" {
				fmt.Fprint(conn, "503 Bad sequence of commands\r\n")
				continue
			}

			recipient := strings.TrimSpace(
				line[len("RCPT TO:"):],
			)

			session.recipients = append(
				session.recipients,
				recipient,
			)

			fmt.Println(
				"SESSION recipients:",
				session.recipients,
			)

			fmt.Fprint(conn, "250 OK\r\n")

		case command == "DATA":
			if len(session.recipients) == 0 {
				fmt.Fprint(conn, "503 Bad sequence of commands\r\n")
				continue
			}

			fmt.Fprint(
				conn,
				"354 End data with <CR><LF>.<CR><LF>\r\n",
			)

			var message strings.Builder

			for {
				dataLine, err := reader.ReadString('\n')
				if err != nil {
					return
				}

				if strings.TrimSpace(dataLine) == "." {
					break
				}

				message.WriteString(dataLine)
			}

			fmt.Println()
			fmt.Println("========== EMAIL RECEIVED ==========")
			fmt.Println("FROM:", session.sender)
			fmt.Println("TO:", session.recipients)
			fmt.Println()
			fmt.Println(message.String())
			fmt.Println("====================================")
			fmt.Println()

			fmt.Fprint(
				conn,
				"250 Message accepted by MailX\r\n",
			)

			session.sender = ""
			session.recipients = nil

		case command == "QUIT":
			fmt.Fprint(conn, "221 Bye\r\n")
			return

		default:
			fmt.Fprint(
				conn,
				"500 Command not recognized\r\n",
			)
		}
	}
}