// Command atmoscheck reports what the configured Atmos client would actually do,
// without needing a database or a running server.
//
// It answers the two questions that are otherwise only answerable by making a
// real payment and watching it fail: is the gateway switched on, and can this
// machine reach Atmos at all.
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/joho/godotenv"

	"prostaff/internal/atmos"
	"prostaff/internal/config"
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("no .env found, using system environment")
	}
	cfg := config.Load()
	c := atmos.New(atmos.Config{
		BaseURL:        cfg.AtmosBaseURL,
		CheckoutURL:    cfg.AtmosCheckoutURL,
		ConsumerKey:    cfg.AtmosConsumerKey,
		ConsumerSecret: cfg.AtmosConsumerSecret,
		StoreID:        cfg.AtmosStoreID,
		TerminalID:     cfg.AtmosTerminalID,
		APIKey:         cfg.AtmosAPIKey,
		RedirectURL:    cfg.AtmosRedirectURL,
		MPSAPIKey:      cfg.AtmosMPSAPIKey,
		MPSStoreID:     cfg.AtmosMPSStoreID,
	})

	fmt.Printf("base_url      %s\n", cfg.AtmosBaseURL)
	fmt.Printf("checkout_url  %s\n", cfg.AtmosCheckoutURL)
	fmt.Printf("store_id      %s\n", cfg.AtmosStoreID)
	fmt.Printf("consumer_key  %s\n", mask(cfg.AtmosConsumerKey))
	fmt.Printf("consumer_sec  %s\n", mask(cfg.AtmosConsumerSecret))
	fmt.Printf("api_key       %s\n", mask(cfg.AtmosAPIKey))
	fmt.Printf("terminal_id   %q\n", cfg.AtmosTerminalID)
	fmt.Printf("redirect_url  %s\n", cfg.AtmosRedirectURL)
	fmt.Printf("callback_ips  %s\n", orNone(cfg.AtmosCallbackIPs))
	fmt.Printf("dev_mode      %v\n", cfg.DevMode)
	fmt.Printf("mps_api_key   %s\n", mask(cfg.AtmosMPSAPIKey))
	fmt.Printf("mps_store_id  %q (empty reuses store_id)\n", cfg.AtmosMPSStoreID)
	fmt.Printf("ENABLED       %v  (UzCard / Humo)\n", c.Enabled())
	fmt.Printf("MPS ENABLED   %v  (Visa / Mastercard)\n", c.MPSEnabled())
	fmt.Printf("sample payurl %s\n", c.InvoiceURL("123456"))

	if c.Enabled() && !c.MPSEnabled() {
		fmt.Println("\nVisa and Mastercard are OFF. Those cards go through Atmos's MPS")
		fmt.Println("module, which is applied for separately and carries a key of its own —")
		fmt.Println("set ATMOS_MPS_API_KEY. Until then an international card is refused with")
		fmt.Println("a sentence saying so, rather than by the merchant API answering \"Karta")
		fmt.Println("topilmadi\" about a card that is perfectly good.")
	}

	if !c.Enabled() {
		fmt.Println("\nGateway OFF — payments take the DEV_MODE path instead. Missing:")
		for _, m := range missing(cfg) {
			fmt.Printf("  - %s\n", m)
		}
		if cfg.AtmosAPIKey == "" {
			fmt.Println("\nATMOS_API_KEY is the key callbacks are signed with, issued when the")
			fmt.Println("Callback API is set up for the store — a separate step from getting")
			fmt.Println("API access. Without it a real callback cannot be told from a forged")
			fmt.Println("one, so the gateway stays off rather than charging cards it could")
			fmt.Println("never credit.")
		}
		return
	}

	fmt.Println("\ncalling Atmos for real...")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	tx, err := c.CreateTransaction(ctx, 10000, "atmoscheck-probe")
	if err != nil {
		fmt.Printf("FAILED %v\n", err)
		fmt.Println("\nA timeout here is almost always the merchant cabinet's IP allowlist")
		fmt.Println("rather than a bad key: Atmos drops unregistered addresses instead of")
		fmt.Println("refusing them. A wrong key comes back as an HTTP 401 straight away.")
		return
	}
	fmt.Printf("OK transaction=%s\npay_url=%s\n", tx.ID, tx.PayURL)
}

// missing names the settings keeping the gateway off, so "why is it not
// enabled" does not require reading Enabled() to work out.
func missing(cfg config.Config) []string {
	var out []string
	for _, f := range []struct{ name, value string }{
		{"ATMOS_BASE_URL", cfg.AtmosBaseURL},
		{"ATMOS_CONSUMER_KEY", cfg.AtmosConsumerKey},
		{"ATMOS_CONSUMER_SECRET", cfg.AtmosConsumerSecret},
		{"ATMOS_STORE_ID", cfg.AtmosStoreID},
		{"ATMOS_API_KEY", cfg.AtmosAPIKey},
	} {
		if f.value == "" {
			out = append(out, f.name)
		}
	}
	return out
}

func orNone(s string) string {
	if s == "" {
		return "(any address — signature is the only gate)"
	}
	return s
}

func mask(s string) string {
	if s == "" {
		return "(empty)"
	}
	if len(s) <= 6 {
		return "***"
	}
	return s[:4] + "..." + s[len(s)-2:] + fmt.Sprintf(" (%d chars)", len(s))
}
