// Package store is a GORM fixture. It is not compiled: testdata is invisible
// to the go tool, which is what lets it depend on gorm without a go.mod entry.
package store

import (
	"context"

	"gorm.io/gorm"
)

// Order states its table outright, so the mapping is certain.
type Order struct {
	ID       int64  `gorm:"primaryKey"`
	Status   string `gorm:"column:status"`
	Merchant string
}

func (Order) TableName() string { return "orders" }

// OrderItem states nothing, so GORM's default naming applies: snake_case then
// pluralised, giving order_items.
type OrderItem struct {
	ID      int64
	OrderID int64
}

type Store struct {
	db *gorm.DB
}

func (s *Store) CreateOrder(ctx context.Context, o *Order) error {
	return s.db.WithContext(ctx).Create(o).Error
}

func (s *Store) FindByMerchant(merchant string) ([]Order, error) {
	var orders []Order
	err := s.db.Where("merchant = ?", merchant).Find(&orders).Error
	return orders, err
}

func (s *Store) MarkPaid(id int64) error {
	return s.db.Model(&Order{}).Where("id = ?", id).Update("status", "PAID").Error
}

func (s *Store) PurgeItems(orderID int64) error {
	return s.db.Where("order_id = ?", orderID).Delete(&OrderItem{}).Error
}

func (s *Store) Migrate() error {
	return s.db.AutoMigrate(&Order{}, &OrderItem{})
}

func (s *Store) CountLegacy() (int64, error) {
	var n int64
	err := s.db.Table("legacy_orders").Count(&n).Error
	return n, err
}

// NotAQuery must produce nothing: Delete on a map is a method name, not a
// database operation, and nothing here is a GORM handle.
func (s *Store) NotAQuery(cache map[string]string) {
	delete(cache, "key")
}
