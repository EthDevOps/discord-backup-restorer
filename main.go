package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/ProtonMail/gopenpgp/v3/crypto"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/pterm/pterm"
	"github.com/russross/blackfriday/v2"
	"html/template"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type directoryInfo struct {
	objectName   string
	lastModified time.Time
}

// Thread represents a Discord thread
type Thread struct {
	ThreadID       int64  `json:"thread_id"`
	ThreadIDHashed string `json:"thread_id_hashed"`
	ThreadName     string `json:"thread_name"`
}

// Channel represents a Discord channel with threads
type Channel struct {
	ChannelID       int64    `json:"channel_id"`
	ChannelIDHashed string   `json:"channel_id_hashed"`
	ChannelName     string   `json:"channel_name"`
	Threads         []Thread `json:"threads"`
}

// Server represents a Discord server with channels
type Server struct {
	ServerID       int64     `json:"server_id"`
	ServerIDHashed string    `json:"server_id_hashed"`
	ServerName     string    `json:"server_name"`
	Channels       []Channel `json:"channels"`
}

type Author struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	GlobalName string `json:"global_name"`
}

// MsgServer Server represents a Discord server
type MsgServer struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// MsgChannel Channel represents a Discord channel
type MsgChannel struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// Attachment represents a file attached to a Discord message
type Attachment struct {
	Type       string `json:"type"`
	OriginName string `json:"origin_name"`
	Content    string `json:"content"` // Base64 encoded content
}

// DiscordMessage represents a complete Discord message
type DiscordMessage struct {
	Author      Author       `json:"author"`
	Server      MsgServer    `json:"server"`
	Channel     MsgChannel   `json:"channel"`
	Category    string       `json:"category"`
	Parent      string       `json:"parent"`
	Content     string       `json:"content"`
	CreatedAt   string       `json:"created_at"` // ISO8601 timestamp as string
	Attachments []Attachment `json:"attachments"`
}

type DiscordExportMessage struct {
	Author      string       `json:"author"`
	Category    string       `json:"category"`
	Parent      string       `json:"parent"`
	Content     string       `json:"content"`
	CreatedAt   string       `json:"created_at"` // ISO8601 timestamp as string
	Attachments []Attachment `json:"attachments"`
}

type HTMLMessage struct {
	DiscordMessage
	FormattedTime       string
	FormattedDate       string
	FormattedContent    template.HTML
	HasAttachments      bool
	ImageAttachments    []HTMLAttachment
	NonImageAttachments []HTMLAttachment
}

// HTMLAttachment extends Attachment with additional fields for HTML display
type HTMLAttachment struct {
	Type       string
	OriginName string
	ContentTag template.HTML // HTML for displaying the attachment
	IsImage    bool
	Size       string
}

type Progress struct {
	channel string
	count   int
}

func main() {

	s3Endpoint := os.Getenv("S3_ENDPOINT")
	s3AccessKey := os.Getenv("S3_ACCESSKEY")
	s3SecretKey := os.Getenv("S3_SECRETKEY")
	s3Bucket := os.Getenv("S3_BUCKET")
	exportDir := os.Getenv("EXPORT_DIR")

	restoreServer := os.Getenv("RESTORE_SERVER")
	restoreChannel := os.Getenv("RESTORE_CHANNEL")
	pathToKey := os.Getenv("GPG_PRIVATE_KEY")

	privkey, err := os.ReadFile(pathToKey)
	if err != nil {
		fmt.Println("Error reading private key:", err)
		return
	}

	// Create a new AWS session
	awsSession, err := session.NewSession(&aws.Config{
		Region:           aws.String("eu-central-1"), // Change to your region
		Endpoint:         aws.String(s3Endpoint),
		Credentials:      credentials.NewStaticCredentials(s3AccessKey, s3SecretKey, ""),
		DisableSSL:       aws.Bool(true),
		S3ForcePathStyle: aws.Bool(true),
	})

	if err != nil {
		fmt.Println("Error creating session:", err)
		return
	}

	// Create S3 service client
	svc := s3.New(awsSession)

	// Find the most recent directory
	fmt.Println("Listing directories...")
	directory := getMostRecentDirectory(s3Bucket, err, svc)

	// Download the directory
	fmt.Println("Downloading latest directory...")
	directoryBytes, err := downloadFileFromS3(s3Bucket, directory.objectName, svc)
	if err != nil {
		fmt.Println("Error downloading file:", err)
		return
	}

	// Decrypt directory
	fmt.Println("Decrypting directory...")
	directoryDecrypted, err := decryptObject(&directoryBytes, string(privkey))
	if err != nil {
		fmt.Println("Error decrypting file:", err)
		return
	}

	// parse JSON
	var servers []Server
	err = json.Unmarshal([]byte(directoryDecrypted), &servers)
	if err != nil {
		log.Fatalf("Error unmarshaling JSON: %v", err)
	}

	// checking for server
	srvToRestore := findServerByName(restoreServer, &servers)

	// Progress go channel
	progressChan := make(chan Progress)

	//totalChannels := len(srvToRestore.Channels)

	// Create a progressbar with the total steps equal to the number of items in fakeInstallList.
	// Set the initial title of the progressbar to "Downloading stuff".

	multi := pterm.DefaultMultiPrinter
	//pr, _ := pterm.DefaultProgressbar.WithTotal(totalChannels).WithTitle("Restoring Channels").WithWriter(multi.NewWriter()).Start()

	var progressStore map[string]*pterm.SpinnerPrinter = make(map[string]*pterm.SpinnerPrinter)
	go func() {
		for p := range progressChan {

			if p.count == -1 {
				progressStore[p.channel].Success("Restored " + p.channel)
				//			pr.Increment()
			} else if p.count == 1 {
				spinner1, _ := pterm.DefaultSpinner.WithWriter(multi.NewWriter()).Start("Restoring " + p.channel)
				progressStore[p.channel] = spinner1
			} else if p.count == 2 {
				progressStore[p.channel].Fail("Restore failed for " + p.channel)
				//			pr.Increment()
			}

		}
	}()

	multi.Start()

	if restoreChannel != "" {
		// restore single channel
		channelToRestore := findChannelInServerByName(restoreChannel, &srvToRestore.Channels)
		exportChannel(s3Bucket, srvToRestore, channelToRestore, svc, exportDir, progressChan, string(privkey))
	} else {
		// restore all channels

		const maxParallelism = 10
		jobs := make(chan ExportJob, 50) // Channel to send jobs
		var wg sync.WaitGroup

		// Start workers
		for w := 1; w <= maxParallelism; w++ {
			wg.Add(1)
			go channelExportWorker(w, jobs, &wg)
		}

		for i := range srvToRestore.Channels {
			c := srvToRestore.Channels[i]
			job := ExportJob{
				s3Svc:           svc,
				server:          srvToRestore,
				channel:         &c,
				exportDir:       exportDir,
				s3Bucket:        s3Bucket,
				progressChannel: progressChan,
				privKey:         string(privkey),
			}
			jobs <- job
		}
		close(jobs)
		wg.Wait()
	}

	multi.Stop()
	fmt.Println("All done!")

}

type ExportJob struct {
	s3Bucket        string
	server          *Server
	channel         *Channel
	s3Svc           *s3.S3
	exportDir       string
	progressChannel chan Progress
	privKey         string
}

func channelExportWorker(id int, jobs <-chan ExportJob, wg *sync.WaitGroup) {
	defer wg.Done()
	for job := range jobs {
		exportChannel(job.s3Bucket, job.server, job.channel, job.s3Svc, job.exportDir, job.progressChannel, job.privKey)
	}
}

func exportChannel(s3Bucket string, srvToRestore *Server, channelToRestore *Channel, svc *s3.S3, exportDir string, channel chan Progress, privKey string) {
	channel <- Progress{channel: channelToRestore.ChannelName, count: 1}
	// Download manifest
	manifestBytes, err := downloadFileFromS3(s3Bucket, "manifests/"+srvToRestore.ServerIDHashed+"-"+channelToRestore.ChannelIDHashed, svc)
	if err != nil {
		fmt.Println("Error downloading manifest for "+channelToRestore.ChannelName+":", err)
		channel <- Progress{channel: channelToRestore.ChannelName, count: 2}
		return
	}

	// Process manifest
	scanner := bufio.NewScanner(bytes.NewReader(manifestBytes))

	var htmlMessages []HTMLMessage

	messagesPerDay := map[string][]DiscordExportMessage{}

	lineNum := 1

	for scanner.Scan() {

		line := scanner.Text()
		lineparts := strings.Split(line, ",")
		ts := lineparts[0]

		// Download message
		msgBytes, err := downloadFileFromS3(s3Bucket, "messages/"+lineparts[1], svc)
		if err != nil {
			fmt.Println("Error downloading message:", err)
			return
		}

		// decrypt message
		msgJson, err := decryptObject(&msgBytes, privKey)
		if err != nil {
			fmt.Println("Error decrypting message:", err)
		}

		// Unmarshal message
		var dcMsg DiscordMessage
		err = json.Unmarshal([]byte(msgJson), &dcMsg)
		if err != nil {
			fmt.Println("Error unmarshaling JSON:", err)
		}

		tParsed, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			fmt.Println("Error parsing time:", err)
			continue
		}

		// JSON export mode
		tFormatted := tParsed.Format(time.DateOnly)

		var exportMsg DiscordExportMessage
		exportMsg.Author = dcMsg.Author.Name
		exportMsg.Content = dcMsg.Content
		exportMsg.CreatedAt = dcMsg.CreatedAt
		exportMsg.Category = dcMsg.Category
		exportMsg.Parent = dcMsg.Parent

		// process attachements
		for a := range dcMsg.Attachments {
			attachment := dcMsg.Attachments[a]

			decodedBytes, err := base64.StdEncoding.DecodeString(attachment.Content)
			if err != nil {
				fmt.Println("Error decoding Base64:", err)
				return
			}

			// Step 2: Hash the decoded byte stream
			hash := sha256.New()
			hash.Write(decodedBytes)
			hashBytes := hash.Sum(nil)
			hashString := hex.EncodeToString(hashBytes)

			// Step 3: Write the content to a file
			filePath := fmt.Sprintf("%s.bin", hashString)
			attachPath := filepath.Join(exportDir, channelToRestore.ChannelName, "attachments")
			err = os.MkdirAll(attachPath, 0755)
			if err != nil {
				fmt.Println("Error creating directory:", err)
				return
			}

			err = os.WriteFile(filepath.Join(attachPath, filePath), decodedBytes, 0644)
			if err != nil {
				fmt.Println("Error writing to file:", err)
				return
			}

			exportMsg.Attachments = append(exportMsg.Attachments, Attachment{
				Type:       attachment.Type,
				OriginName: attachment.OriginName,
				Content:    hashString,
			})
		}

		messagesPerDay[tFormatted] = append(messagesPerDay[tFormatted], exportMsg)

		// HTML Export mode
		htmlMsg := convertToHTMLMessage(dcMsg)
		htmlMessages = append(htmlMessages, htmlMsg)

		lineNum++
	}

	templateData := struct {
		ServerName  string
		ChannelName string
		Messages    []HTMLMessage
	}{
		ServerName:  htmlMessages[0].Server.Name,
		ChannelName: htmlMessages[0].Channel.Name,
		Messages:    htmlMessages,
	}

	// Create the HTML file
	fullPathHtml := filepath.Join(exportDir, "html")
	err = os.MkdirAll(fullPathHtml, 0755)
	err = generateHTML(templateData, filepath.Join(fullPathHtml, channelToRestore.ChannelName+".html"))
	if err != nil {
		log.Fatalf("Error generating HTML: %v", err)
	}

	// report done
	channel <- Progress{channel: channelToRestore.ChannelName, count: -1}

	// write out date based files

	// make dirs
	fullPath := filepath.Join(exportDir, channelToRestore.ChannelName)
	err = os.MkdirAll(fullPath, 0755)
	if err != nil {
		log.Fatalf("Error creating directory: %v", err)
	}

	for day := range messagesPerDay {
		dayJson, err := json.MarshalIndent(messagesPerDay[day], "", "    ")
		if err != nil {
			fmt.Println("Error marshaling JSON:", err)
			continue
		}
		// create file
		filePath := filepath.Join(fullPath, day+".json")
		err = os.WriteFile(filePath, dayJson, 0644)
		if err != nil {
			fmt.Println("Error writing file:", err)
		}
	}
}

func findChannelInServerByName(channel string, channels *[]Channel) *Channel {
	for _, c := range *channels {
		if c.ChannelName == channel {
			return &c
		}
	}
	return nil
}

func findServerByName(serverName string, servers *[]Server) *Server {
	for _, srv := range *servers {
		if srv.ServerName == serverName {
			return &srv
		}
	}
	return nil
}

func decryptObject(payload *[]byte, privateKeyAsc string) (string, error) {
	pgp := crypto.PGP()

	// Load private keyt from disk
	var passphrase = []byte(``) // Passphrase of the privKey
	privateKey, err := crypto.NewPrivateKeyFromArmored(privateKeyAsc, passphrase)
	if err != nil {
		return "", err
	}

	decHandle, err := pgp.Decryption().DecryptionKey(privateKey).New()
	decrypted, err := decHandle.Decrypt(*payload, crypto.Armor)
	myMessage := decrypted.Bytes()

	decHandle.ClearPrivateParams()

	return string(myMessage), nil
}

func downloadFileFromS3(bucket string, objectName string, svc *s3.S3) ([]byte, error) {

	input := &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(objectName),
	}

	result, err := svc.GetObject(input)
	if err != nil {
		return nil, fmt.Errorf("error downloading file from S3: %v", err)
	}
	defer result.Body.Close()
	return io.ReadAll(result.Body)

}

func getMostRecentDirectory(s3Bucket string, err error, svc *s3.S3) directoryInfo {
	// Get all directories
	// Initial request parameters
	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(s3Bucket),
		Prefix: aws.String("directories/"),
	}

	var directories []directoryInfo = []directoryInfo{}

	// Process all pages
	err = svc.ListObjectsV2Pages(input, func(page *s3.ListObjectsV2Output, lastPage bool) bool {
		for _, item := range page.Contents {
			// Ignore seals
			if !strings.Contains(*item.Key, "/seals/") {
				directories = append(directories, directoryInfo{*item.Key, *item.LastModified})
			}

		}
		// Return true to continue pagination, false to stop
		return true
	})

	if err != nil {
		log.Fatalf("Error listing objects: %v", err)
	}

	fmt.Println("Found ", len(directories), "directories")

	// Find latest directory
	sort.Slice(directories, func(i, j int) bool {
		return directories[i].lastModified.After(directories[j].lastModified)
	})

	var mostRecentDirectory directoryInfo
	if len(directories) > 0 {
		mostRecentDirectory = directories[0]
		fmt.Printf("Using most recent directory: %s\n", mostRecentDirectory.objectName)
	} else {
		fmt.Println("No directories found")
	}
	return mostRecentDirectory
}
func convertToHTMLMessage(msg DiscordMessage) HTMLMessage {
	// Parse the time
	t, err := time.Parse(time.RFC3339, msg.CreatedAt)
	if err != nil {
		log.Printf("Warning: couldn't parse time '%s': %v", msg.CreatedAt, err)
		t = time.Now() // Fallback
	}

	// Format time for display
	formattedTime := t.Format("15:04")
	formattedDate := t.Format("January 2, 2006")

	// Process content (convert markdown-like syntax to HTML)
	formattedContent := formatMessageContent(msg.Content)

	// Process attachments
	var imageAttachments []HTMLAttachment
	var nonImageAttachments []HTMLAttachment

	for _, attachment := range msg.Attachments {
		htmlAttachment := processAttachment(attachment)

		if htmlAttachment.IsImage {
			imageAttachments = append(imageAttachments, htmlAttachment)
		} else {
			nonImageAttachments = append(nonImageAttachments, htmlAttachment)
		}
	}

	return HTMLMessage{
		DiscordMessage:      msg,
		FormattedTime:       formattedTime,
		FormattedDate:       formattedDate,
		FormattedContent:    template.HTML(formattedContent),
		HasAttachments:      len(msg.Attachments) > 0,
		ImageAttachments:    imageAttachments,
		NonImageAttachments: nonImageAttachments,
	}
}

// formatMessageContent converts Discord-style markdown to HTML
func formatMessageContent(content string) string {

	html := blackfriday.Run([]byte(content))

	return string(html)
}

// processAttachment converts an Attachment to HTMLAttachment for display
func processAttachment(attachment Attachment) HTMLAttachment {
	isImage := strings.HasPrefix(attachment.Type, "image/")
	var contentTag string

	if isImage {
		// For images, embed directly if content is base64
		contentTag = fmt.Sprintf(`<img src="data:%s;base64, %s" alt="%s" class="attachment-image"/>`,
			attachment.Type, attachment.Content, attachment.OriginName)
	} else {
		// For other files, provide a download link or embedded viewer based on type
		contentTag = fmt.Sprintf(`<div class="attachment-file">
			<div class="attachment-icon">📄</div>
			<div class="attachment-info">
				<div class="attachment-filename">%s</div>
				<div class="attachment-filesize">Document</div>
			</div>
		</div>`, attachment.OriginName)
	}

	return HTMLAttachment{
		Type:       attachment.Type,
		OriginName: attachment.OriginName,
		ContentTag: template.HTML(contentTag),
		IsImage:    isImage,
		Size:       "Unknown size", // In a real app, calculate the size
	}
}

func generateHTML(data interface{}, filename string) error {
	// HTML template with CSS styling to resemble Discord
	const htmlTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Discord Messages - {{.ServerName}} / #{{.ChannelName}}</title>
    <style>
        :root {
            --discord-dark: #36393f;
            --discord-light: #ffffff;
            --discord-gray: #72767d;
            --discord-channel: #8e9297;
            --discord-blurple: #5865F2;
            --discord-green: #3ba55c;
            --discord-attachment-bg: #2f3136;
            --discord-hover: rgba(79, 84, 92, 0.16);
            --discord-divider: rgba(79, 84, 92, 0.48);
            --discord-message-hover: rgba(4, 4, 5, 0.07);
        }
        
        body {
            font-family: 'Whitney', 'Helvetica Neue', Helvetica, Arial, sans-serif;
            background-color: var(--discord-dark);
            color: var(--discord-light);
            margin: 0;
            padding: 0;
            line-height: 1.5;
            font-size: 16px;
        }
        
        .discord-container {
            max-width: 1200px;
            margin: 0 auto;
            padding: 16px;
        }
        
        .discord-header {
            display: flex;
            align-items: center;
            padding: 8px 16px;
            border-bottom: 1px solid var(--discord-divider);
            margin-bottom: 16px;
        }
        
        .channel-name {
            font-size: 16px;
            font-weight: bold;
            color: var(--discord-light);
        }
        
        .channel-hash {
            font-size: 24px;
            color: var(--discord-gray);
            margin-right: 8px;
        }
        
        .messages-container {
            padding: 8px 0;
        }
        
        .message {
            padding: 8px 16px;
            display: flex;
            margin-bottom: 4px;
            border-radius: 4px;
            transition: background-color 0.1s;
        }
        
        .message:hover {
            background-color: var(--discord-message-hover);
        }
        
        .message-avatar {
            width: 40px;
            height: 40px;
            border-radius: 50%;
            background-color: var(--discord-blurple);
            color: white;
            display: flex;
            align-items: center;
            justify-content: center;
            font-weight: bold;
            margin-right: 16px;
            flex-shrink: 0;
        }
        
        .message-content-wrapper {
            flex-grow: 1;
            min-width: 0;
        }
        
        .message-header {
            display: flex;
            align-items: baseline;
            margin-bottom: 4px;
        }
        
        .message-author {
            font-weight: 500;
            margin-right: 8px;
            color: white;
        }
        
        .message-timestamp {
            font-size: 0.75rem;
            color: var(--discord-gray);
        }
        
        .message-content {
            word-wrap: break-word;
            color: #dcddde;
        }
        
        .message-content a {
            color: #00aff4;
            text-decoration: none;
        }
        
        .message-content a:hover {
            text-decoration: underline;
        }
        
        .attachments {
            margin-top: 8px;
            display: flex;
            flex-direction: column;
            gap: 8px;
        }
        
        .attachment-image {
            max-width: 400px;
            max-height: 300px;
            border-radius: 4px;
        }
        
        .attachment-file {
            background-color: var(--discord-attachment-bg);
            border-radius: 4px;
            padding: 16px;
            display: flex;
            align-items: center;
            max-width: 400px;
        }
        
        .attachment-icon {
            font-size: 24px;
            margin-right: 12px;
        }
        
        .attachment-filename {
            font-weight: 500;
            color: var(--discord-light);
        }
        
        .attachment-filesize {
            font-size: 0.75rem;
            color: var(--discord-gray);
        }
        
        .date-divider {
            margin: 16px 0;
            text-align: center;
            font-size: 0.8rem;
            color: var(--discord-gray);
            position: relative;
        }
        
        .date-divider::before,
        .date-divider::after {
            content: "";
            height: 1px;
            background-color: var(--discord-divider);
            position: absolute;
            top: 50%;
            width: 40%;
        }
        
        .date-divider::before {
            left: 0;
        }
        
        .date-divider::after {
            right: 0;
        }
    </style>
</head>
<body>
    <div class="discord-container">
        <div class="discord-header">
            <span class="channel-hash">#</span>
            <span class="channel-name">{{.ChannelName}}</span>
        </div>
        
        <div class="messages-container">
            {{$prevDate := ""}}
            {{range .Messages}}
                {{if ne $prevDate .FormattedDate}}
                    <div class="date-divider">{{.FormattedDate}}</div>
                    {{$prevDate = .FormattedDate}}
                {{end}}
                
                <div class="message">
                    <div class="message-avatar">
                        {{slice .Author.Name 0 1}}
                    </div>
                    <div class="message-content-wrapper">
                        <div class="message-header">
                            <span class="message-author">{{.Author.Name}}</span>
                            <span class="message-timestamp">{{.FormattedTime}}</span>
                        </div>
                        <div class="message-content">
                            {{.FormattedContent}}
                        </div>
                        
                        {{if .HasAttachments}}
                            <div class="attachments">
                                {{range .ImageAttachments}}
                                    <div class="attachment">
                                        {{.ContentTag}}
                                    </div>
                                {{end}}
                                
                                {{range .NonImageAttachments}}
                                    <div class="attachment">
                                        {{.ContentTag}}
                                    </div>
                                {{end}}
                            </div>
                        {{end}}
                    </div>
                </div>
            {{end}}
        </div>
    </div>
</body>
</html>`

	// Create a template
	tmpl, err := template.New("discord").Parse(htmlTemplate)
	if err != nil {
		return fmt.Errorf("error parsing template: %w", err)
	}

	// Create the output file
	file, err := os.Create(filename)
	if err != nil {
		return fmt.Errorf("error creating file: %w", err)
	}
	defer file.Close()

	// Execute the template
	err = tmpl.Execute(file, data)
	if err != nil {
		return fmt.Errorf("error executing template: %w", err)
	}

	return nil
}
