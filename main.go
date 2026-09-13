package main

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"strings"
)

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
			fmt.Fprint(conn, "250-localhost\r\n")
			fmt.Fprint(conn, "250 OK\r\n")

		case strings.HasPrefix(command, "HELO"):
			fmt.Fprint(conn, "250 localhost\r\n")

		case strings.HasPrefix(command, "MAIL FROM:"):
			fmt.Fprint(conn, "250 OK\r\n")

		case strings.HasPrefix(command, "RCPT TO:"):
			fmt.Fprint(conn, "250 OK\r\n")

		case command == "DATA":
			fmt.Fprint(conn, "354 End data with <CR><LF>.<CR><LF>\r\n")

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
			fmt.Println(message.String())
			fmt.Println("====================================")
			fmt.Println()

			fmt.Fprint(conn, "250 Message accepted by MailX\r\n")

		case command == "QUIT":
			fmt.Fprint(conn, "221 Bye\r\n")
			return

		default:
			fmt.Fprint(conn, "500 Command not recognized\r\n")
		}
	}
}