package main

import (
	// --- Standard Library ---
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	// --- Third-Party Library ---
	"github.com/gin-gonic/gin"
	"github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"
)

// Global map penyimpan sesi WA
var clientMap = make(map[string]*whatsmeow.Client)

// ==============================================================================
// FUNGSI PELAPOR WEBHOOK KE CI3
// ==============================================================================
func kirimWebhookKeCI3(sessionID string, statusKoneksi string) {
	webhookURL := "http://localhost/halonotif/api/webhook_device"
	apiKey := "RAHASIA_NEGARA_123"

	payload := map[string]string{
		"session_id": sessionID,
		"status":     statusKoneksi,
	}
	jsonData, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", webhookURL, bytes.NewBuffer(jsonData))
	if err != nil {
		fmt.Println("Gagal merakit webhook:", err)
		return
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", apiKey)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Println("Gagal mengirim webhook ke CI3:", err)
		return
	}
	defer resp.Body.Close()

	fmt.Printf("[WEBHOOK] Session %s melapor ke CI3 -> %s\n", sessionID, statusKoneksi)
}

// ==============================================================================
// FUNGSI 1: AUTO-RESTORE (Saat server Golang baru dinyalakan)
// ==============================================================================
func restoreSessions(container *sqlstore.Container) {
	devices, err := container.GetAllDevices(context.Background())
	if err != nil {
		fmt.Println("Gagal mengambil daftar device dari database:", err)
		return
	}

	for _, device := range devices {
		nomorWA := device.ID.User
		fmt.Println("🚀 Membangunkan kembali device WA:", nomorWA)

		clientLog := waLog.Stdout("Client", "ERROR", true)
		client := whatsmeow.NewClient(device, clientLog)

		clientMap[nomorWA] = client

		// EVENT HANDLER 1 (Untuk Restore Session)
		client.AddEventHandler(func(evt interface{}) {
			switch evt.(type) {
			case *events.Connected:
				fmt.Println("[EVENT] WA Connected for:", nomorWA)
				kirimWebhookKeCI3(nomorWA, "Connected")

			case *events.Disconnected:
				fmt.Println("[EVENT] WA Disconnected for:", nomorWA)
				kirimWebhookKeCI3(nomorWA, "Disconnected")

			case *events.LoggedOut:
				fmt.Println("[EVENT] WA Logged Out for:", nomorWA)
				kirimWebhookKeCI3(nomorWA, "Disconnected")
				client.Logout(context.Background())
			}
		})

		errConnect := client.Connect()
		if errConnect != nil {
			fmt.Println("Gagal menyambungkan", nomorWA, ":", errConnect)
		}
	}
}

// ==============================================================================
// MAIN FUNCTION & API ENDPOINTS
// ==============================================================================
func main() {
	dbLog := waLog.Stdout("Database", "ERROR", true)
	container, err := sqlstore.New(context.Background(), "sqlite", "file:halonotif_sessions.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", dbLog)
	if err != nil {
		panic(err)
	}

	// Panggil Auto-Restore saat server nyala
	restoreSessions(container)

	gin.SetMode(gin.ReleaseMode)
	router := gin.Default()

	// Middleware API Key
	router.Use(func(c *gin.Context) {
		token := c.GetHeader("Authorization")
		if token != "Bearer RAHASIA_NEGARA_123" {
			c.JSON(http.StatusUnauthorized, gin.H{"status": "error", "pesan": "Unauthorized Access"})
			c.Abort()
			return
		}
		c.Next()
	})

	// --- ENDPOINT: START SESSION (Scan QR) ---
	router.POST("/api/session/start", func(c *gin.Context) {
		var req struct {
			SessionID  string `json:"session_id" binding:"required"`
			WebhookURL string `json:"webhook_url" binding:"required"`
		}
		if errBind := c.ShouldBindJSON(&req); errBind != nil {
			c.JSON(http.StatusBadRequest, gin.H{"status": "error", "pesan": "Format request salah"})
			return
		}

		if existingClient, ok := clientMap[req.SessionID]; ok {
			existingClient.Disconnect()
		}

		deviceStore := container.NewDevice()
		clientLog := waLog.Stdout("Client", "ERROR", true)
		client := whatsmeow.NewClient(deviceStore, clientLog)

		// Simpan sementara pakai ID CI3 saat baru mau scan
		clientMap[req.SessionID] = client

		// EVENT HANDLER 2 (Untuk Session Baru / QR Scan)
		client.AddEventHandler(func(evt interface{}) {
			switch evt.(type) {
			case *events.Connected:
				nomorWA := ""
				if client.Store.ID != nil {
					nomorWA = client.Store.ID.User
					// Begitu sukses connect, simpan juga pakai Nomor WA biar permanen
					clientMap[nomorWA] = client
				}
				fmt.Println("[EVENT] WA Connected for session:", req.SessionID, "Number:", nomorWA)
				kirimWebhookKeCI3(req.SessionID, "Connected")

			case *events.Disconnected:
				fmt.Println("[EVENT] WA Disconnected for session:", req.SessionID)
				kirimWebhookKeCI3(req.SessionID, "Disconnected")

			case *events.LoggedOut:
				fmt.Println("[EVENT] WA Logged Out for session:", req.SessionID)
				kirimWebhookKeCI3(req.SessionID, "Disconnected")
				client.Logout(context.Background())
			}
		})

		if client.Store.ID == nil {
			qrChan, _ := client.GetQRChannel(context.Background())
			errConnect := client.Connect()
			if errConnect != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "pesan": "Gagal konek"})
				return
			}
			qrData := <-qrChan
			png, _ := qrcode.Encode(qrData.Code, qrcode.Medium, 256)
			base64QR := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)

			c.JSON(http.StatusOK, gin.H{"status": "success", "data": gin.H{"qr_code": base64QR}})
		} else {
			client.Connect()
			c.JSON(http.StatusOK, gin.H{"status": "error", "pesan": "Session sudah login"})
		}
	})

	// --- ENDPOINT: CEK STATUS REAL-TIME ---
	router.POST("/api/session/status", func(c *gin.Context) {
		var req struct {
			SessionID string `json:"session_id"`
			Sender    string `json:"sender"`
		}

		if errBind := c.ShouldBindJSON(&req); errBind != nil {
			c.JSON(http.StatusBadRequest, gin.H{"status": "error", "pesan": "Format data tidak lengkap"})
			return
		}

		client, ok := clientMap[req.Sender]
		if !ok {
			client, ok = clientMap[req.SessionID]
		}

		isConnected := false
		if ok && client != nil {
			isConnected = client.IsConnected() && client.IsLoggedIn()
		}

		statusStr := "Disconnected"
		if isConnected {
			statusStr = "Connected"
		}

		c.JSON(http.StatusOK, gin.H{
			"status":         "success",
			"status_koneksi": statusStr,
		})
	})

	// --- ENDPOINT: KIRIM PESAN TEKS & GAMBAR ---
	router.POST("/api/message/send", func(c *gin.Context) {
		var req struct {
			SessionID string `json:"session_id"`
			Sender    string `json:"sender"`
			To        string `json:"to"`
			Text      string `json:"text"`
			Type      string `json:"type"`
			ImageURL  string `json:"image_url"`
		}

		if errBind := c.ShouldBindJSON(&req); errBind != nil {
			c.JSON(http.StatusBadRequest, gin.H{"status": "error", "pesan": "Format data tidak lengkap"})
			return
		}

		client, ok := clientMap[req.Sender]
		if !ok {
			client, ok = clientMap[req.SessionID]
		}

		if !ok || client == nil || !client.IsConnected() {
			c.JSON(http.StatusNotFound, gin.H{"status": "error", "pesan": "Device tidak terhubung"})
			return
		}

		// =================================================================
		// FITUR BARU: RADAR PENGECEK NOMOR AKTIF
		// =================================================================
		// Formatkan nomor tujuan ke dalam array string
		targetWA := []string{req.To}

		// Tanya ke server Meta, apakah nomor ini ada WA-nya?
		resWA, errWA := client.IsOnWhatsApp(context.Background(), targetWA)
		if errWA != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "pesan": "Gagal memvalidasi status nomor WA"})
			return
		}

		// Jika response kosong ATAU nomor tersebut IsIn = false (tidak terdaftar)
		if len(resWA) == 0 || !resWA[0].IsIn {
			c.JSON(http.StatusBadRequest, gin.H{"status": "error", "pesan": "Nomor tidak terdaftar di WhatsApp"})
			return
		}

		// Gunakan JID resmi dari hasil pengecekan Meta (Lebih aman dari targetJID manual)
		targetJID := resWA[0].JID
		// =================================================================

		var msg *waE2E.Message

		// JIKA PESAN GAMBAR
		if req.Type == "image" && req.ImageURL != "" {
			imgResp, errGet := http.Get(req.ImageURL)
			if errGet != nil || imgResp.StatusCode != 200 {
				c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "pesan": "Golang gagal mendownload gambar dari CI3"})
				return
			}
			defer imgResp.Body.Close()
			imgData, _ := io.ReadAll(imgResp.Body)

			uploaded, errUp := client.Upload(context.Background(), imgData, whatsmeow.MediaImage)
			if errUp != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "pesan": "Gagal mengunggah media ke WhatsApp"})
				return
			}

			msg = &waE2E.Message{
				ImageMessage: &waE2E.ImageMessage{
					Caption:       proto.String(req.Text),
					URL:           proto.String(uploaded.URL),
					DirectPath:    proto.String(uploaded.DirectPath),
					MediaKey:      uploaded.MediaKey,
					Mimetype:      proto.String(http.DetectContentType(imgData)),
					FileEncSHA256: uploaded.FileEncSHA256,
					FileSHA256:    uploaded.FileSHA256,
					FileLength:    proto.Uint64(uint64(len(imgData))),
				},
			}
		} else {
			// JIKA PESAN TEKS BIASA
			msg = &waE2E.Message{
				Conversation: proto.String(req.Text),
			}
		}

		// 4. Tembak pesan
		_, errSend := client.SendMessage(context.Background(), targetJID, msg)
		if errSend != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "pesan": errSend.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"status": "success", "pesan": "Pesan berhasil dikirim"})
	})

	// --- RUN SERVER ---
	go func() {
		fmt.Println("🚀 HaloNotif WA Engine berjalan di Port 3000...")
		router.Run(":3000")
	}()

	// --- GRACEFUL SHUTDOWN ---
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	fmt.Println("\nMematikan engine WA...")
	for _, client := range clientMap {
		client.Disconnect()
	}
}
