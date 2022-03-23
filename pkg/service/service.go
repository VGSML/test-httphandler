package service

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// unlimited безлимитное значение параметра.
	unlimited = 0
	// defURLRequestTimeout таймаут при запросе URL по умолчанию.
	defURLRequestTimeout = 10 * time.Second
	// defJobBuffers размер буферизированных каналов заданий и результатов запросов.
	defJobBuffers = 100
	// defCacheCleaningPeriod период очистки кеша.
	defCacheCleaningPeriod = 1800 * time.Second
)

var (
	wrongRequestErr = errors.New("неверный формат запроса данных")
	nothingToDoErr  = errors.New("не переданы URL для подсчета размера ответов")
)

// Service служба обработки http запросов на определение размера ответов по перечню URL,
// реализует интерфейс http.Handler.
// обрабатывает запрос POST, в теле которого строки с url для определения размера ответа запроса http.Get
// возвращает ответ в виде текста, в строках которого размер в байтах ответов для каждого URL.
type Service struct {
	requests   int32
	httpClient *http.Client

	requestsLimit       uint
	maxJobs             int
	urlRequestTimeout   time.Duration
	cacheLifetime       time.Duration
	jobsBuffersSize     int
	cacheCleaningPeriod time.Duration

	mux   sync.Mutex
	cache map[string]cacheItem

	stopJobs context.CancelFunc
	jobCh    chan jobItem
}

// ServiceOpt опции службы.
type ServiceOpt func(s *Service)

// WithRequestLimit установка лимита на количество одновременных запросов.
func WithRequestLimit(limit uint) ServiceOpt {
	return func(s *Service) {
		s.requestsLimit = limit
	}
}

// WithLimitedJobs установка лимита на количество потоков (горутин) на запрос URL
// и размер буфера каналов заданий и результатов.
func WithLimitedJobs(jobs, bufferSize int) ServiceOpt {
	return func(s *Service) {
		s.maxJobs = jobs
		s.jobsBuffersSize = bufferSize
	}
}

// WithURLRequestTimeout установка таймаута на запрос URL.
func WithURLRequestTimeout(limit time.Duration) ServiceOpt {
	return func(s *Service) {
		s.urlRequestTimeout = limit
	}
}

// WithCacheLifetime установка времени жизни размера сообщения в кеше
func WithCacheLifetime(limit time.Duration) ServiceOpt {
	return func(s *Service) {
		s.cacheLifetime = limit
	}
}

// WithCacheCleaningPeriod устанавливает период очистки кеша.
func WithCacheCleaningPeriod(limit time.Duration) ServiceOpt {
	return func(s *Service) {
		s.cacheCleaningPeriod = limit
	}
}

// WithHTTPClient устанавливает http.Client определения размера ответов по URL.
func WithHTTPClient(client *http.Client) ServiceOpt {
	return func(s *Service) {
		s.httpClient = client
	}
}

// cacheItem кешированное значение размера ответа и времени запроса.
type cacheItem struct {
	time time.Time
	size uint
}

// jobItem задание на определение размера ответа по URL.
type jobItem struct {
	url   string          // ссылка
	ctx   context.Context // контекст запроса
	resCh chan<- resItem  // канал результатов
	errCh chan<- error    // канал ошибок
}

// resItem результат определения размера ответа.
type resItem struct {
	url  string    // ссылка
	size uint      // размер ответа в байтах
	time time.Time // время запроса
}

// New конструктор службы.
// Принимает опции.
func New(opts ...ServiceOpt) *Service {
	s := &Service{
		urlRequestTimeout:   defURLRequestTimeout,
		cache:               make(map[string]cacheItem),
		jobsBuffersSize:     defJobBuffers,
		cacheCleaningPeriod: defCacheCleaningPeriod,
		httpClient:          http.DefaultClient,
	}

	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}

	if s.maxJobs != unlimited {
		s.jobCh = make(chan jobItem, s.jobsBuffersSize)
	}
	return s
}

// Start осуществляет инициализацию рабочих потоков.
func (s *Service) Start(ctx context.Context) {
	if s.maxJobs == unlimited {
		return
	}
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.stopJobs != nil {
		return
	}
	s.jobCh = make(chan jobItem, s.jobsBuffersSize)
	var jobCtx context.Context
	jobCtx, s.stopJobs = context.WithCancel(ctx)
	for i := 0; i < s.maxJobs; i++ {
		go s.startWorkingJob(jobCtx)
	}
	if s.cacheLifetime != unlimited {
		go s.cacheCleaning(ctx)
	}
}

// Stop безопасно останавливает выполнение запросов.
func (s *Service) Stop() {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.stopJobs != nil {
		s.stopJobs()
	}
}

// Обработка HTTP запроса.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Accept, Content-Type")
	if r.Method == http.MethodOptions {
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}
	// контроль лимитов одновременных подключений
	if s.requestsLimit != unlimited && uint(atomic.LoadInt32(&s.requests)) >= s.requestsLimit {
		http.Error(w, "превышен лимит запросов", http.StatusTooManyRequests)
		return
	}
	atomic.AddInt32(&s.requests, 1)
	defer atomic.AddInt32(&s.requests, -1)

	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// определение размеров ответов для запроса
	sizes, err := s.processBody(r.Context(), data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// формирование ответа
	var out []string
	for _, size := range sizes {
		out = append(out, strconv.FormatUint(uint64(size), 10))
	}

	_, err = w.Write([]byte(strings.Join(out, "\n")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// processBody обрабатывает данные запроса и возвращает результат в виде слайса с размерами ответов.
func (s *Service) processBody(ctx context.Context, body []byte) ([]uint, error) {
	if body == nil {
		return nil, wrongRequestErr
	}
	urls := strings.Split(string(body), "\n")
	if len(urls) == 0 {
		return nil, nothingToDoErr
	}

	// получение значений из кеша
	var urlsForRequest []string
	var out []uint
	if s.cacheLifetime != unlimited {
		s.mux.Lock()
		for _, url := range urls {
			if val, ok := s.cache[url]; ok && time.Since(val.time) <= s.cacheLifetime {
				out = append(out, val.size)
				continue
			}
			urlsForRequest = append(urlsForRequest, url)
		}
		s.mux.Unlock()
	}
	if s.cacheLifetime == unlimited {
		urlsForRequest = urls
	}

	// получение запроса по сети
	if len(urlsForRequest) == 0 {
		return out, nil
	}

	wg := new(sync.WaitGroup)
	resCh := make(chan resItem, defJobBuffers)
	defer close(resCh)
	errCh := make(chan error)
	defer close(errCh)

	var cancel context.CancelFunc
	ctx, cancel = context.WithCancel(ctx)
	defer cancel() // завершаем контекст после получения результатов.

	// формируем задания на обработку
	wg.Add(len(urlsForRequest))
	go func() {
		for _, url := range urlsForRequest {
			select {
			case <-ctx.Done():
				return
			default:
				if url != "" {
					s.addJob(jobItem{
						url:   url,
						ctx:   ctx,
						resCh: resCh,
						errCh: errCh,
					})
				}
			}
		}
	}()

	// получаем результаты
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case res, ok := <-resCh:
				if !ok {
					return
				}
				out = append(out, res.size)
				if s.cacheLifetime != unlimited {
					s.mux.Lock()
					s.cache[res.url] = cacheItem{
						size: res.size,
						time: res.time,
					}
					s.mux.Unlock()
				}
				wg.Done()
			}
		}
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		done <- struct{}{}
	}()

	// ожидаем завершения или ошибку, после завершения обработки контекст
	// завершится
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-done:
			return out, nil
		case err := <-errCh:
			return nil, err
		}
	}
}

// startWorkingJob запускает рабочий поток обработки.
func (s *Service) startWorkingJob(ctx context.Context) {
	for job := range s.jobCh {
		select {
		case <-ctx.Done():
			return
		default:
			s.processJob(job)
		}
	}
}

// addJob добавление потока обработки
func (s *Service) addJob(job jobItem) {
	if s.maxJobs == unlimited {
		go s.processJob(job)
		return
	}
	s.jobCh <- job
}

// processJob выполнение задания.
func (s *Service) processJob(job jobItem) {
	size, err := s.urlResponseSize(job.ctx, job.url, s.urlRequestTimeout)
	select {
	case <-job.ctx.Done():
		return
	default:
		if err != nil {
			job.errCh <- err
			return
		}
		job.resCh <- resItem{
			url:  job.url,
			size: uint(size),
			time: time.Now(),
		}
	}
}

// cacheCleaning очистка кеша от устаревших результатов.
func (s *Service) cacheCleaning(ctx context.Context) {
	for {
		<-time.After(s.cacheCleaningPeriod)
		select {
		case <-ctx.Done():
			return
		default:
			s.mux.Lock()
			for url, item := range s.cache {
				if time.Since(item.time) >= s.cacheLifetime {
					delete(s.cache, url)
				}
			}
			s.mux.Unlock()
		}
	}
}

// urlResponseSize возвращает размер ответа.
func (s *Service) urlResponseSize(ctx context.Context, url string, timeout time.Duration) (int, error) {
	if timeout != unlimited {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	res, err := s.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	if res.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("%v return %v", url, res.Status)
	}
	b, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}
