package handlers

import (
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/caiyeon/goldfish/vault"
	"github.com/gorilla/csrf"
	"github.com/gorilla/securecookie"
	"github.com/labstack/echo"
)

// for returning JSON bodies
type H map[string]interface{}

// for storing ciphers of user credentials
var scookie = &securecookie.SecureCookie{}

func init() {
	// setup cookie encryption keys
	hashKey := securecookie.GenerateRandomKey(64)
	blockKey := securecookie.GenerateRandomKey(32)
	if hashKey == nil || blockKey == nil {
		panic("Failed to generate random hashkey")
	}
	scookie = securecookie.New(hashKey, blockKey)
	scookie = scookie.MaxAge(14400) // 8 hours
	if scookie == nil {
		panic("Failed to initialize gorilla/securecookie")
	}
}

// returns the http status code found in the error message
func parseError(c echo.Context, err error) error {
	errCode := strings.Split(err.Error(), "Code:")
	errMsgs := strings.Split(err.Error(), "*")

	// if error string did not contain error response code
	if len(errCode) < 2 || len(errMsgs) < 2 {
		log.Println("[ERROR]: ", err.Error())
		return c.JSON(http.StatusInternalServerError, H{
			"error": "Invalid vault response",
		})
	}

	code := 500
	fmt.Sscanf(errCode[1], "%d", &code)
	return c.JSON(code, H{
		"error": "Vault: " + errMsgs[1],
	})
}

func FetchCSRF() echo.HandlerFunc {
	return func(c echo.Context) error {
		c.Response().Writer.Header().Set("X-CSRF-Token", csrf.Token(c.Request()))
		return c.JSON(http.StatusOK, H{
			"status": "fetched",
		})
	}
}

func VaultHealth() echo.HandlerFunc {
	return func(c echo.Context) error {
		resp, err := vault.VaultHealth()
		if err != nil {
			return parseError(c, err)
		}
		return c.JSON(http.StatusOK, H{
			"result": string(resp),
		})
	}
}

func Login() echo.HandlerFunc {
	return func(c echo.Context) error {
		auth := new(vault.AuthInfo)
		defer auth.Clear()

		// read form data
		if err := c.Bind(auth); err != nil {
			return c.JSON(http.StatusBadRequest, H{
				"error": "Invalid auth format",
			})
		}
		if auth.Type == "" || auth.ID == "" {
			return c.JSON(http.StatusBadRequest, H{
				"error": "Empty authentication",
			})
		}

		// verify auth details and create client access token
		data, err := auth.Login()
		if err != nil {
			return parseError(c, err)
		}

		// encrypt auth.ID with vault's transit backend
		if err := auth.EncryptAuth(); err != nil {
			return c.JSON(http.StatusInternalServerError, H{
				"error": "Goldfish could not use transit key",
			})
		}

		// store auth.Type and auth.ID (now a cipher) in cookie
		if encoded, err := scookie.Encode("auth", auth); err == nil {
			cookie := &http.Cookie{
				Name:  "auth",
				Value: encoded,
				Path:  "/",
			}
			http.SetCookie(c.Response().Writer, cookie)
		} else {
			return c.JSON(http.StatusInternalServerError, H{
				"error": "Goldfish could not encode cookie",
			})
		}

		// return useful information to user
		return c.JSON(http.StatusOK, H{
			"status": "Logged in",
			"data": map[string]interface{}{
				"display_name": data["display_name"],
				"id":           data["id"],
				"meta":         data["meta"],
				"policies":     data["policies"],
				"renewable":    data["renewable"],
				"ttl":          data["ttl"],
			},
		})
	}
}

func RenewSelf() echo.HandlerFunc {
	return func(c echo.Context) error {
		// fetch auth from header or cookie
		auth := getSession(c)
		if auth == nil {
			return nil
		}
		defer auth.Clear()

		// verify auth details and create client access token
		resp, err := auth.RenewSelf()
		if err != nil {
			return parseError(c, err)
		}

		return c.JSON(http.StatusOK, H{
			"data": map[string]interface{}{
				"meta":     resp.Auth.Metadata,
				"policies": resp.Auth.Policies,
				"ttl":      resp.Auth.LeaseDuration,
			},
		})
	}
}

// if session is valid (prefer headers over cookie), returns true
// otherwise, writes errors to response, and returns false
func getSession(c echo.Context) (*vault.AuthInfo) {
	var auth = &vault.AuthInfo{}

	// check headers first
	if token := c.Request().Header.Get("X-Vault-Token"); token != "" {
		auth.Type = "token"
		auth.ID = token
		return auth
	}

	// fetch auth from cookie
	cookie, err := c.Request().Cookie("auth")
	if err != nil {
		c.JSON(http.StatusForbidden, H{
			"error": "Please login first",
		})
		return nil
	}
	if err := scookie.Decode("auth", cookie.Value, &auth); err != nil {
		c.JSON(http.StatusForbidden, H{
			"error": "Please login first",
		})
		return nil
	}

	// if cookie is valid, decrypt it with transit key
	if err := auth.DecryptAuth(); err != nil {
		parseError(c, err)
		return nil
	}
	return auth
}
