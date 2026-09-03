package main

import (
	"context"
	"fmt"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type Order struct {
	ID     uint   `gorm:"primaryKey"`
	Status string `gorm:"not null;default:'new'"`
}

type printer struct{ logger.Interface }

func (printer) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	sql, _ := fc()
	fmt.Println(sql + ";")
}

func main() {
	// The DSN is parsed for the dialect and never connected to: DryRun builds the SQL and executes nothing.
	db, err := gorm.Open(
		postgres.New(postgres.Config{DSN: "postgres://godwit@localhost:5432/unused"}),
		&gorm.Config{DryRun: true, Logger: printer{logger.Discard}},
	)
	if err != nil {
		panic(err)
	}
	if err := db.Migrator().CreateTable(&Order{}); err != nil {
		panic(err)
	}
}
