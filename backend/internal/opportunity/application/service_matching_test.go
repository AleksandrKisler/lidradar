package application

import (
	"testing"
	"time"

	catalogdomain "lidradar/backend/internal/catalog/domain"
)

func TestMatchingServicesRussianCaseForms(t *testing.T) {
	for _, test := range []struct {
		name, text string
		want       bool
	}{
		{"Полировка", "Здравствуйте! Хочу записаться на полировку завтра в 16:00. Запишите меня, пожалуйста.", true},
		{"Полировка", "Какая стоимость полировки?", true},
		{"Полировка", "Вопрос по полировке", true},
		{"Полировка", "Помогите с полировкой", true},
		{"Полировка", "ПОЛИРОВКА!", true},
		{"Полировка кузова", "Хочу записаться на полировку кузова", true},
		{"Лазерная эпиляция", "Запишите на лазерную эпиляцию", true},
		{"Лазерная эпиляция", "Сколько стоит сеанс лазерной эпиляции?", true},
		{"Мужская стрижка", "Хочу на мужскую стрижку", true},
		{"Восстановление волос", "Подскажите по восстановлению волос", true},
		{"Массаж", "Запишите на сеанс массажа", true},
		{"Спортивный массаж", "После спортивного массажа можно тренироваться?", true},
		{"Маникюр", "Интересуюсь маникюром", true},
		{"Химический пилинг", "Вопрос о химическом пилинге", true},
		{"Диагностика", "Цена диагностики?", true},
		{"SPA уход", "Хочу SPA-уход", true},
		{"Полировка", "Нужен полировщик", false},
		{"Полировка", "Нужна переполировка", false},
		{"Полировка", "Можно отполировать кузов?", false}, // Глаголы и синонимы не угадываются.
		{"Полировка", "Запишите на палировку", false},     // Опечатки не исправляются.
		{"Полировка кузова", "Хочу полировку фар", false},
		{"Полировка кузова", "Нужна полировка", false},
		{"Лазерная эпиляция", "Лазерная процедура, а эпиляция потом", false},
		{"Лазерная эпиляция", "Эпиляция лазерная", false},
		{"R16", "Нужен R17", false},
		{"SPA", "Нужен SPARK", false},
		{"МРТ", "Что такое МРТа?", false},
		{"R16", "Нужен R16", true},
		{"Полировка", "Где скачать старый чек?", false},
		{"Полировка", "", false},
	} {
		t.Run(test.name+"/"+test.text, func(t *testing.T) {
			item, err := catalogdomain.NewServiceCatalogItem("service", "tenant", test.name, nil, nil, nil, "RUB", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			matched := matchingServices(test.text, nil, []catalogdomain.ServiceCatalogItem{item})
			if got := len(matched) == 1; got != test.want {
				t.Fatalf("matchingServices(%q, %q) matched=%v, want=%v", test.text, test.name, got, test.want)
			}
		})
	}
}

func TestMatchingServicesKeepsAllAmbiguousMatchesAndLocationScope(t *testing.T) {
	location, otherLocation := "location", "other-location"
	makeItem := func(id, name string, where *string, active bool) catalogdomain.ServiceCatalogItem {
		t.Helper()
		item, err := catalogdomain.NewServiceCatalogItem(id, "tenant", name, where, nil, nil, "RUB", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		item.Active = active
		return item
	}
	items := []catalogdomain.ServiceCatalogItem{
		makeItem("global", "Полировка", nil, true),
		makeItem("local", "Полировка кузова", &location, true),
		makeItem("foreign", "Полировка", &otherLocation, true),
		makeItem("inactive", "Полировка", nil, false),
	}
	matched := matchingServices("Запишите на полировку кузова", &location, items)
	if len(matched) != 2 || matched[0].ID != "global" || matched[1].ID != "local" {
		t.Fatalf("ambiguity/location filtering lost: %#v", matched)
	}
	matched = matchingServices("Нужна полировка кузова", nil, items)
	if len(matched) != 1 || matched[0].ID != "global" {
		t.Fatalf("location-specific service matched unscoped conversation: %#v", matched)
	}
	// Exact spelling must not win over another service's case form.
	items = append(items[:1], makeItem("exact", "Полировки", nil, true))
	if matched = matchingServices("Стоимость полировки?", nil, items); len(matched) != 2 {
		t.Fatalf("exact match hid ambiguity: %#v", matched)
	}
}
