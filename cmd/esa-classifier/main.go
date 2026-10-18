package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"esa/internal/classifier"
)

func read(path string) ([]classifier.Example, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return classifier.ReadExamples(f)
}
func run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: esa-classifier train|test|predict|retrain [flags]")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	data := fs.String("data", "", "Reviewed JSONL dataset")
	model := fs.String("model", "", "Model file for test/predict")
	output := fs.String("output", "", "New candidate model file (must not exist)")
	feedback := fs.String("feedback", "", "Human feedback JSONL export for retrain")
	cutoff := fs.String("cutoff", "", "Required training/retraining cutoff, RFC3339")
	text := fs.String("text", "", "Ticket text for independent prediction")
	o := classifier.DefaultOptions()
	labels := fs.String("labels", "", "Optional comma-separated complete label set")
	fs.IntVar(&o.MinDF, "min-df", o.MinDF, "Minimum document frequency")
	fs.IntVar(&o.MaxFeatures, "max-features", o.MaxFeatures, "Vocabulary limit")
	fs.IntVar(&o.Epochs, "epochs", o.Epochs, "Batch gradient iterations")
	fs.Float64Var(&o.LearningRate, "learning-rate", o.LearningRate, "Gradient step, (0,1]")
	fs.Float64Var(&o.L2, "l2", o.L2, "L2 weight penalty")
	fs.Float64Var(&o.Threshold, "threshold", o.Threshold, "Independent label suggestion threshold")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if *labels != "" {
		o.Labels = strings.Split(*labels, ",")
		for i := range o.Labels {
			o.Labels[i] = strings.TrimSpace(o.Labels[i])
		}
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	switch args[0] {
	case "train", "retrain":
		if *data == "" || *output == "" || *cutoff == "" {
			return fmt.Errorf("data, output and cutoff are required")
		}
		at, err := time.Parse(time.RFC3339, *cutoff)
		if err != nil {
			return err
		}
		if at.After(time.Now()) {
			return fmt.Errorf("cutoff cannot be in the future")
		}
		es, err := read(*data)
		if err != nil {
			return err
		}
		if args[0] == "retrain" {
			if *feedback == "" {
				return fmt.Errorf("feedback is required for retrain")
			}
			fb, err := read(*feedback)
			if err != nil {
				return err
			}
			es, err = classifier.MergeFeedback(es, fb, at)
			if err != nil {
				return err
			}
		} else {
			for _, e := range es {
				if e.ReviewedAt.After(at) {
					return fmt.Errorf("review for %s is after cutoff", e.ID)
				}
			}
		}
		m, err := classifier.Train(ctx, es, o)
		if err != nil {
			return err
		}
		if err = m.Save(*output); err != nil {
			return err
		}
		return enc.Encode(map[string]any{
			"model":            *output,
			"version":          m.Version,
			"training_count":   m.TrainingCount,
			"training_through": m.TrainingThrough,
			"labels":           m.Labels(),
			"features":         len(m.Vocabulary),
			"activation":       "Manual test and promotion required",
		})
	case "test":
		if *model == "" || *data == "" {
			return fmt.Errorf("model and data are required")
		}
		m, err := classifier.Load(*model)
		if err != nil {
			return err
		}
		es, err := read(*data)
		if err != nil {
			return err
		}
		r, err := m.Evaluate(es)
		if err != nil {
			return err
		}
		return enc.Encode(r)
	case "predict":
		if *model == "" || strings.TrimSpace(*text) == "" {
			return fmt.Errorf("model and text are required")
		}
		if len(*text) > 1<<20 {
			return fmt.Errorf("text exceeds 1 MB")
		}
		m, err := classifier.Load(*model)
		if err != nil {
			return err
		}
		return enc.Encode(m.Predict(*text))
	default:
		return fmt.Errorf("unknown command %s", args[0])
	}
}
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
