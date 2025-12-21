# Reddit Gallery DL 🚀

A high-performance, memory-efficient web tool to download Reddit galleries. Written in Go, it streams ZIP archives on-the-fly, ensuring low memory usage even for massive galleries.

## ✨ Features

-   **🚀 Zero-Allocation Streaming:** Downloads are streamed directly from Reddit to the client's ZIP file. Server RAM usage stays near ~10MB.
-   **📱 Mobile First:** Fully responsive UI with a touch-friendly grid and **Dark Mode** support.
-   **🔒 Smart Validation:** Automatically handles shortened URLs (`redd.it`), NSFW warnings, and redirects.
-   **⚡ Production Ready:**
    -   Context-aware cancellation (stops downloads if tab closes).
    -   Tiny Docker image (~15MB) based on Alpine Linux.
    -   Robust error handling.
-   **🎨 Modern UI:** Built with Bootstrap 5, featuring sticky toolbars and loading states.

## 🛠️ Quick Start

### 🐳 Using Docker (Recommended)

The easiest way to run the application:

```bash
docker build -t reddit-gallery-dl .
docker run -p 5000:5000 reddit-gallery-dl
```

Visit **http://localhost:5000**.

### 💻 Local Development

Requires **Go 1.22+**.

```bash
# Clone the repository
git clone https://github.com/dacrab/reddit-gallery-dl.git
cd reddit-gallery-dl

# Run the server
go run .
```

## ☁️ Deployment

Perfect for PaaS providers like **Render**, **Railway**, or **Fly.io**.

1.  Fork this repository.
2.  Connect your GitHub account to your hosting provider.
3.  Create a new Web Service in your dashboard.
4.  **Done!** (The platform will automatically detect the `Dockerfile`).

*Note: The app listens on `$PORT` (defaults to 5000).*

## 🏗️ Architecture

-   **`reddit.go`**: Encapsulated API client with typed errors and context support.
-   **`handlers.go`**: HTTP layer managing templates, validation, and ZIP streaming.
-   **`main.go`**: Configuration and server startup.

## 📄 License

MIT