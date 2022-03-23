package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"

	"github.com/VGSML/test-httphandler/pkg/service"
)

var (
	fAddr                = flag.String("listen-addr", ":8080", "адрес сервера")
	fRequestLimit        = flag.Uint("request-limit", 0, "максимальное количество параллельных запросов")
	fURLTimeout          = flag.Duration("url-timeout", 0, "таймаут при получении ответа по URL")
	fURLRequestLimit     = flag.Uint("url-request-limit", 0, "максимальное параллельное количество запросов URL")
	fBufferSize          = flag.Uint("buffer-size", 0, "размер буферов обработки URL")
	fCacheLifetime       = flag.Duration("cache-lifetime", 0, "максимальное время жизни в кеше)")
	fCacheCleaningPeriod = flag.Duration("cache-cleaning-period", 0, "период очистки кеша")
)

func main() {
	flag.Parse()
	var opts []service.ServiceOpt
	if *fRequestLimit != 0 {
		opts = append(opts, service.WithRequestLimit(*fRequestLimit))
	}
	if *fURLTimeout != 0 {
		opts = append(opts, service.WithURLRequestTimeout(*fURLTimeout))
	}
	if *fURLRequestLimit != 0 {
		opts = append(opts, service.WithLimitedJobs(int(*fURLRequestLimit), int(*fBufferSize)))
	}
	if *fCacheLifetime != 0 {
		opts = append(opts, service.WithCacheLifetime(*fCacheLifetime))
	}
	if *fCacheCleaningPeriod != 0 {
		opts = append(opts, service.WithCacheCleaningPeriod(*fCacheCleaningPeriod))
	}
	s := service.New(opts...)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill)
	defer stop()
	s.Start(ctx)
	log.Println("Сервер запущен на адресе", *fAddr)

	go func() {
		err := http.ListenAndServe(*fAddr, s)
		if err != nil {
			log.Fatal("ошибка запуска сервера", err)
		}
		stop()
	}()
	<-ctx.Done()
}
