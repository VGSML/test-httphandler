package service

import (
	"bytes"
	"context"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func Test_getURLResponseSize(t *testing.T) {
	s := New()
	tests := []struct {
		name    string
		url     string
		timeout time.Duration
		wantErr bool
	}{
		{
			name:    "google",
			url:     "https://www.google.com",
			timeout: 10 * time.Second,
		},
		{
			name:    "wikipedia",
			url:     "https://wikipedia.org",
			timeout: time.Millisecond,
			wantErr: true,
		},
		{
			name:    "wrong url",
			url:     "test err",
			timeout: time.Millisecond,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.urlResponseSize(context.Background(), tt.url, tt.timeout)
			if (err != nil) != tt.wantErr {
				t.Errorf("getURLResponseSize() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			t.Log(got)
		})
	}
}

func TestNew(t *testing.T) {
	tests := []struct {
		name string
		opts []ServiceOpt
	}{
		{
			name: "default",
		},
		{
			name: "With jobs",
			opts: []ServiceOpt{
				WithLimitedJobs(100, 100),
				WithRequestLimit(100),
				WithURLRequestTimeout(20),
			},
		},
		{
			name: "With jobs",
			opts: []ServiceOpt{
				WithLimitedJobs(100, 100),
				WithRequestLimit(100),
				WithURLRequestTimeout(20),
				WithCacheLifetime(time.Second * 20),
			},
		},
		{
			name: "With jobs",
			opts: []ServiceOpt{
				WithLimitedJobs(100, 100),
				WithRequestLimit(100),
				WithURLRequestTimeout(20),
				WithCacheLifetime(time.Second * 20),
				WithCacheCleaningPeriod(time.Second),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(tt.opts...)
			s.Start(context.Background())
			s.Stop()
			t.Log("ok")
		})
	}
}

func TestService_ServeHTTP(t *testing.T) {
	tests := []struct {
		name string
		opts []ServiceOpt
		body string
	}{
		{
			name: "default",
			body: "https://google.com\nhttps://wikipedia.com\nhttps://developer.mozilla.org\nhttps://youtube.com",
		},
		{
			name: "With jobs",
			opts: []ServiceOpt{
				WithLimitedJobs(10, 100),
				WithRequestLimit(2),
				WithURLRequestTimeout(20 * time.Second),
			},
			body: "https://google.com\nhttps://wikipedia.com\nhttps://developer.mozilla.org\nhttps://youtube.com",
		},
		{
			name: "With jobs, with cache 10",
			opts: []ServiceOpt{
				WithLimitedJobs(4, 10),
				WithRequestLimit(10),
				WithURLRequestTimeout(20 * time.Second),
				WithCacheLifetime(time.Second),
			},
			body: "https://google.com\nhttps://wikipedia.com\nhttps://developer.mozilla.org\nhttps://youtube.com",
		},
		{
			name: "With jobs, with cache 100",
			opts: []ServiceOpt{
				WithLimitedJobs(2, 100),
				WithRequestLimit(1000),
				WithURLRequestTimeout(20 * time.Second),
				WithCacheLifetime(20 * time.Second),
				WithCacheCleaningPeriod(10 * time.Second),
			},
			body: "https://google.com\nhttps://wikipedia.com\nhttps://developer.mozilla.org\nhttps://youtube.com",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(tt.opts...)
			s.Start(context.Background())
			defer s.Stop()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// проверка выполнения запроса
			r := httptest.NewRequest(http.MethodPost, "/", bytes.NewBuffer([]byte(tt.body)))
			r = r.WithContext(ctx)
			res := httptest.NewRecorder()
			s.ServeHTTP(res, r)
			if res.Result().StatusCode != http.StatusOK {
				t.Error("ошибка запроса", res.Result().Status)
			}
			b, _ := ioutil.ReadAll(res.Result().Body)
			t.Log(string(b))
			// Проверка на количество запросов
			wg := new(sync.WaitGroup)
			wg.Add(int(s.requestsLimit) + 1)
			var codes int32
			s.cacheLifetime = 0 // для проверки параллельных запросов выключаем кеш
			for i := 0; i < int(s.requestsLimit)+1; i++ {
				go func() {
					r := httptest.NewRequest(http.MethodPost, "/", bytes.NewBuffer([]byte(tt.body)))
					r = r.WithContext(ctx)
					res := httptest.NewRecorder()
					s.ServeHTTP(res, r)
					if res.Result().StatusCode == http.StatusTooManyRequests {
						atomic.AddInt32(&codes, 1)
						cancel()
					}
					wg.Done()
				}()
			}
			wg.Wait()
			if codes == 0 && s.requestsLimit != unlimited {
				t.Error("контроль количества запросов не выполнен")
			}
		})
	}
}
