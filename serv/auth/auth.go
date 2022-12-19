// Package auth provides an API to use GraphJin serv auth handles with your own application. Works with routers like chi and http mux.
// For detailed documentation visit https://graphjin.com
//
// Example usage:
/*
	package main

	import (
		"net/http"
		"path/filepath"
		"github.com/go-chi/chi"
		"github.com/dosco/graphjin/v2/serv"
		"github.com/dosco/graphjin/v2/serv/auth"
	)

	func main() {
		conf, err := serv.ReadInConfig(filepath.Join("./config", serv.GetConfigName()))
		if err != nil {
			panic(err)
		}

		useAuth, err := auth.NewAuth(conf.Auth, log, auth.Options{AuthFailBlock: true})
		if err != nil {
			panic(err)
		}

		r := chi.NewRouter()
		r.Use(useAuth)
		r.Get("/user", userInfo)

		http.ListenAndServe(":8080", r)
	}
*/
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	core "github.com/dosco/graphjin/v2/core"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/dosco/graphjin/v2/serv/auth/provider"
)

var ErrNoAuthDefined = errors.New("no auth defined")

type JWTConfig = provider.JWTConfig

// Auth struct contains authentication related config values used by the GraphJin service
type Auth struct {
	// Enable development mode used to set credentials in the header and vars for testing
	Development bool `jsonschema:"title=Development Mode,default=false"`

	// Name is a friendly name for this auth config
	Name string

	// Type can be one of rails, jwt or header
	Type string `jsonschema:"title=Type,enum=jwt,enum=rails,enum=header"`

	// The name of the cookie that holds the authentication token
	Cookie string `jsonschema:"title=Cookie Name"`

	// In certain cases like Magiclink the jwt cookie is generated by us this
	// set the secure parameter of this cookie
	// CookieHTTPS bool `mapstructure:"cookie_https"`

	// In certain cases like Magiclink the jwt cookie is generated by us this
	// set the expiry parameter of this cookie (ex. "20m", "2h")
	// CookieExpiry string `mapstructure:"cookie_expiry"`

	// Ruby on Rails cookie authentication
	Rails struct {
		// Rails version is needed to decode the cookie correctly.
		// Can be 5.2 or 6
		Version string `jsonschema:"enum=5.2,enum=6"`

		// SecretKeyBase is the cookie encryption key used in your Rails config
		SecretKeyBase string `mapstructure:"secret_key_base"`

		// URL is used for Rails cookie store based auth.
		// Example: redis://redis-host:6379 or memcache://memcache-host
		URL string `jsonschema:"title=Cookie Store URL,example=redis://redis-host:6379"`

		// Password is set if needed by the cookie store (Redis, Memcache, etc)
		Password string

		// Maximum idle time for the connection
		MaxIdle int `mapstructure:"max_idle" jsonschema:"title=Cookie Store Maximum Idle Time"`

		// MaxActive maximum active time for the connection
		MaxActive int `mapstructure:"max_active" jsonschema:"title=Cookie Store Maximum Active Time"`

		// Salt value is from your Rails 5.2 and below auth config
		Salt string

		// SignSalt value is from your Rails 5.2 and below auth config
		SignSalt string `mapstructure:"sign_salt" jsonschema:"title=Siging Salt (Rails 5.2)"`

		// AuthSalt value is from your Rails 5.2 and below auth config
		AuthSalt string `mapstructure:"auth_salt" jsonschema:"title=Authentication Salt (Rails 5.2)"`
	}

	// JWT authentication
	JWT JWTConfig

	// Header authentication
	Header struct {
		// Name of the HTTP header
		Name string

		// Value if set must match expected value (optional)
		Value string

		// Exists if set to true then the header must exist
		// this is an alternative to using value
		Exists bool
	}

	// Magic.link authentication
	// MagicLink struct {
	// 	Secret string
	// }
}

type HandlerFunc func(w http.ResponseWriter, r *http.Request) (context.Context, error)

type Options struct {
	// Return a HTTP '401 Unauthoized' when auth fails
	AuthFailBlock bool
}

// NewAuthHandlerFunc returns a HandlerFunc based on the provided config.
// Usually you don't need to use this function, because is called by NewAuth if
// no HandlerFunc is provided.
func NewAuthHandlerFunc(ac Auth) (HandlerFunc, error) {
	var h HandlerFunc
	var err error

	switch ac.Development {
	case true:
		h, err = SimpleHandler(ac)

	default:
		switch ac.Type {
		case "rails":
			h, err = RailsHandler(ac)

		case "jwt":
			h, err = JwtHandler(ac)

		case "header":
			h, err = HeaderHandler(ac)

		// case "magiclink":
		// 	h, err = MagicLinkHandler(ac, next)
		case "", "none":
			return nil, ErrNoAuthDefined

		default:
			return nil, fmt.Errorf("auth: unknown auth type: %s", ac.Type)
		}

		if err != nil {
			return nil, fmt.Errorf("%s: %s", ac.Type, err.Error())
		}
	}
	return h, err
}

// NewAuth returns a new auth handler. It will create a HandlerFunc based on the
// provided config.
//
// Optionally an existing HandlerFunc can be provided. This is required to
// support auth in WS subscriptions.
func NewAuth(ac Auth, log *zap.Logger, opt Options, hFn ...HandlerFunc) (
	func(next http.Handler) http.Handler, error) {
	var err error
	var h HandlerFunc
	var wsAuthSupported bool

	if len(hFn) != 0 && hFn[0] != nil {
		h = hFn[0]
		wsAuthSupported = true
	} else {
		h, err = NewAuthHandlerFunc(ac)
		if err != nil {
			return nil, err
		}
	}

	return func(next http.Handler) http.Handler {
		ah := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if wsAuthSupported && websocket.IsWebSocketUpgrade(r) {
				next.ServeHTTP(w, r)
				return
			}
			c, err := h(w, r)
			if err != nil && log != nil {
				log.Error("Auth", []zapcore.Field{zap.String("type", ac.Type), zap.Error(err)}...)
			}

			if err == Err401 {
				http.Error(w, "401 unauthorized", http.StatusUnauthorized)
				return
			}

			if opt.AuthFailBlock && !IsAuth(c) {
				http.Error(w, "401 unauthorized", http.StatusUnauthorized)
				return
			}

			if c != nil {
				next.ServeHTTP(w, r.WithContext(c))
			} else {
				next.ServeHTTP(w, r)
			}
		})

		return ah
	}, nil
}

func SimpleHandler(ac Auth) (HandlerFunc, error) {
	return func(_ http.ResponseWriter, r *http.Request) (context.Context, error) {
		c := r.Context()

		userIDProvider := r.Header.Get("X-User-ID-Provider")
		if userIDProvider != "" {
			c = context.WithValue(c, core.UserIDProviderKey, userIDProvider)
		}

		userID := r.Header.Get("X-User-ID")
		if userID != "" {
			c = context.WithValue(c, core.UserIDKey, userID)
		}

		userRole := r.Header.Get("X-User-Role")
		if userRole != "" {
			c = context.WithValue(c, core.UserRoleKey, userRole)
		}

		return c, nil
	}, nil
}

var Err401 = errors.New("401 unauthorized")

func HeaderHandler(ac Auth) (HandlerFunc, error) {
	hdr := ac.Header

	if hdr.Name == "" {
		return nil, fmt.Errorf("auth '%s': no header.name defined", ac.Name)
	}

	if !hdr.Exists && hdr.Value == "" {
		return nil, fmt.Errorf("auth '%s': no header.value defined", ac.Name)
	}

	return func(_ http.ResponseWriter, r *http.Request) (context.Context, error) {
		var fo1 bool
		value := r.Header.Get(hdr.Name)

		switch {
		case hdr.Exists:
			fo1 = (value == "")

		default:
			fo1 = (value != hdr.Value)
		}

		if fo1 {
			return nil, Err401
		}
		return nil, nil
	}, nil
}

func IsAuth(c context.Context) bool {
	return c.Value(core.UserIDKey) != nil
}

func UserID(c context.Context) interface{} {
	return c.Value(core.UserIDKey)
}

func UserIDInt(c context.Context) int {
	v, ok := UserID(c).(string)
	if !ok {
		return -1
	}
	if v, err := strconv.Atoi(v); err == nil {
		return v
	}
	return -1
}
