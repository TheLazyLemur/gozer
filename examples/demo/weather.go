package main

import (
	"math/rand/v2"
	"time"
)

type Forecast struct {
	Date    time.Time
	TempC   int
	Summary string
}

func (f Forecast) TempF() int { return 32 + f.TempC*9/5 }

// WeatherService is a singleton injected into the Weather page.
type WeatherService struct{ summaries []string }

func NewWeatherService() *WeatherService {
	return &WeatherService{summaries: []string{"Freezing", "Bracing", "Chilly", "Cool", "Mild", "Warm", "Balmy", "Hot", "Sweltering", "Scorching"}}
}

// Forecasts simulates a slow backend call.
func (s *WeatherService) Forecasts(n int) []Forecast {
	time.Sleep(400 * time.Millisecond)
	out := make([]Forecast, n)
	for i := range out {
		out[i] = Forecast{
			Date:    time.Now().AddDate(0, 0, i+1),
			TempC:   rand.IntN(75) - 20,
			Summary: s.summaries[rand.IntN(len(s.summaries))],
		}
	}
	return out
}
