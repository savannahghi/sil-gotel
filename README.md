# **Observability SDK for Go Services**

The **Observability SDK** is a one-time setup library designed to bring **out-of-the-box observability** to all your Go microservices. By initializing this SDK at the earliest point in your service startup, you ensure consistent, standardized, and reliable observability across your entire ecosystem.

> ⚠️ **Important:**
> Your service **SHALL NOT** start if an error occurs during SDK initialization. This ensures that every running service has full observability enabled — no exceptions.

---

## **What This SDK Provides**

* **Unified Observability Setup**
  Automatically configures metrics, tracing, and structured logging with zero boilerplate.

* **Standardized Telemetry Across All Services**
  Every service that uses this SDK exposes consistent telemetry formats and behaviors.

* **Fail-Fast Initialization**
  If anything goes wrong during setup, your service exits early — preventing unmonitored services from running.

* **Simple, One-Time Setup**
  Initialize once at startup and the SDK configures the rest.

* **Extensible by Design**
  Built to allow service-specific overrides without losing global standards.

---

## 📦 **Installation**

```bash
go get github.com/savannahghi/sil-gotel
```

---

## 🧩 **Usage**

### 1. **Import the SDK**

```go
import "github.com/savannahghi/sil-gotel"
```

### 2. **Initialize at the Earliest Point of Startup**

Place the initialization **before your service spins up anything** — even before routers, DB connections, or workers.

```go
otelClient := &silotel.Client{
		OTLPBaseURL: serverutils.MustGetEnvVar("OTLP_ENDPOINT"),
		ServiceName: "your-service-name",
		Environment: serverutils.GetRunningEnvironment(),
		Version:     "your-version",
	}

	_, err := silotel.NewOtelSDK(ctx, otelClient)
	if err != nil {
		log.Error("❌ could not init Open Telemetry", "error", err)
		return err
	}
```

### 3. **You're All Set!**

Once initialized, the SDK handles telemetry setup and instrumentation automatically.

---

## 🛑 **Fail Fast Philosophy**

This SDK adheres to strict correctness:

* If the SDK cannot properly initialize telemetry
* If configuration is missing or malformed
* If required exporters cannot start

👉 **The service will NOT start.**
This enforces consistent, reliable observability for all deployed services.

---

## 🔧 **Configuration**

The SDK reads configuration from environment variables such as:

| Variable                | Description                                |
| ----------------------- | ------------------------------------------ |
| `OTLP_ENDPOINT` | Endpoint for exporting traces and metrics  |
| `ENVIRONMENT`               | Environment tag (`testing`, `staging`, `prod`) |

---

## **Why This Matters**

Observability isn’t optional — it's foundational. This library:

* Eliminates setup inconsistencies
* Reduces observability drift across services
* Allows engineers to focus on business logic
* Ensures every service is monitored, traceable, and diagnosable

**Happy Observing 👀👀👀**

---

## 🤝 **Contributing**

1. Clone the repo
2. Create your feature branch
3. Submit a PR

---

## 📄 **License**

MIT

---