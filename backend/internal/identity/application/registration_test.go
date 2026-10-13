package application

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"lidradar/backend/internal/identity/domain"
)

// Регистрация по приглашению (ADR 0057): режим, два вида кодов, одинаковый отказ для любой причины
// и отсутствие следов у несостоявшейся регистрации.

const (
	platformCode = "P0000000000000000000000000000000000000000AA"
	memberCode   = "M0000000000000000000000000000000000000000AA"
)

func TestFixtureCodesHaveTheProductionFormat(t *testing.T) {
	for _, code := range []string{platformCode, memberCode} {
		if !invitationCodePattern.MatchString(code) {
			t.Fatalf("тестовый код %q не соответствует формату кодов", code)
		}
	}
}

// memberGate подменяет модуль tenant: коды сотрудников и исход приёма задаются тестом.
type memberGate struct {
	usable    map[string]bool
	accepted  map[string]string
	acceptErr error
	checkErr  error
	accepts   int
}

func newMemberGate(codes ...string) *memberGate {
	gate := &memberGate{usable: map[string]bool{}, accepted: map[string]string{}}
	for _, code := range codes {
		gate.usable[code] = true
	}
	return gate
}

func (g *memberGate) CheckMemberInvitation(_ context.Context, code string) error {
	if g.checkErr != nil {
		return g.checkErr
	}
	if !g.usable[code] {
		return ErrInvitationInvalid
	}
	return nil
}

func (g *memberGate) AcceptMemberInvitation(_ context.Context, userID, code string) error {
	g.accepts++
	if g.acceptErr != nil {
		return g.acceptErr
	}
	if !g.usable[code] {
		return ErrInvitationInvalid
	}
	g.usable[code] = false
	g.accepted[code] = userID
	return nil
}

type fixture struct {
	repository *memoryRepository
	service    Service
	gate       *memberGate
}

func newFixture(mode RegistrationMode, gate *memberGate) fixture {
	repository := newMemoryRepository()
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	tokens := &testTokens{}
	repository.platform[tokens.HashToken(platformCode)] = true
	service := NewService(repository, newTestRateLimiter(), testPasswords{}, &testIDs{}, tokens, func() time.Time { return now }, time.Hour)
	if gate != nil {
		service = service.WithRegistration(mode, gate)
	} else {
		service = service.WithRegistration(mode, nil)
	}
	return fixture{repository: repository, service: service, gate: gate}
}

func domainUser(email string) domain.User {
	return domain.User{ID: "existing-" + email, Email: email, PasswordHash: "hashed:x", DisplayName: "Существующий", Status: domain.UserActive}
}

func registration(email, code string) RegistrationInput {
	return RegistrationInput{Email: email, Password: "very-secure-password", DisplayName: "Новый", InviteCode: code}
}

func (f fixture) userCount() int { return len(f.repository.users) }

func TestClosedRegistrationRequiresAnInvitation(t *testing.T) {
	for name, mode := range map[string]RegistrationMode{"invite_only": RegistrationInviteOnly, "не настроен": ""} {
		f := newFixture(mode, nil)
		if _, err := f.service.Register(context.Background(), registration("new@example.com", ""), Client{}); !errors.Is(err, ErrInvitationRequired) {
			t.Errorf("%s: регистрация без кода = %v, want ErrInvitationRequired", name, err)
		}
		if f.userCount() != 0 {
			t.Errorf("%s: отказ оставил пользователя", name)
		}
	}
}

func TestInvalidCodesGetTheSameRefusalAndLeaveNoTrace(t *testing.T) {
	f := newFixture(RegistrationInviteOnly, newMemberGate(memberCode))
	f.repository.platform[(&testTokens{}).HashToken("X0000000000000000000000000000000000000000AA")] = false // просрочен или отозван
	for name, code := range map[string]string{
		"неверный формат":                "short",
		"слишком длинный":                strings.Repeat("A", 44),
		"неизвестный код":                "Z0000000000000000000000000000000000000000AA",
		"недействительный код платформы": "X0000000000000000000000000000000000000000AA",
	} {
		if _, err := f.service.Register(context.Background(), registration("new@example.com", code), Client{}); !errors.Is(err, ErrInvitationInvalid) {
			t.Errorf("%s: %v, want ErrInvitationInvalid", name, err)
		}
	}
	if f.userCount() != 0 || f.gate.accepts != 0 {
		t.Fatalf("отказы оставили следы: пользователей %d, приёмов %d", f.userCount(), f.gate.accepts)
	}
}

func TestPlatformCodeCreatesTheUserWithOneOrganizationRightAndIsSingleUse(t *testing.T) {
	f := newFixture(RegistrationInviteOnly, newMemberGate())
	registered, err := f.service.Register(context.Background(), registration("owner@example.com", "  "+platformCode+"\n"), Client{})
	if err != nil {
		t.Fatalf("регистрация по коду платформы: %v", err)
	}
	user := f.repository.users["owner@example.com"]
	if user.ID != registered.User.ID || user.OrganizationRights != 1 {
		t.Fatalf("пользователь %+v, ожидалось одно право на организацию", user)
	}
	if f.repository.consumedBy[(&testTokens{}).HashToken(platformCode)] != user.ID {
		t.Fatal("код платформы не погашен этим пользователем")
	}
	if _, err := f.service.Register(context.Background(), registration("second@example.com", platformCode), Client{}); !errors.Is(err, ErrInvitationInvalid) {
		t.Fatalf("повторное использование кода = %v, want ErrInvitationInvalid", err)
	}
	if f.userCount() != 1 {
		t.Fatalf("пользователей %d, ожидался один", f.userCount())
	}
}

// Занятая почта не должна сжигать код: оператор не выпускает новый из-за опечатки в адресе.
func TestEmailConflictDoesNotConsumeAPlatformCode(t *testing.T) {
	f := newFixture(RegistrationOpen, nil)
	if _, err := f.service.Register(context.Background(), registration("owner@example.com", ""), Client{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Register(context.Background(), registration("OWNER@example.com", platformCode), Client{}); !errors.Is(err, ErrEmailConflict) {
		t.Fatalf("регистрация на занятую почту = %v, want ErrEmailConflict", err)
	}
	if usable, _ := f.repository.PlatformInvitationUsable(context.Background(), (&testTokens{}).HashToken(platformCode), time.Now()); !usable {
		t.Fatal("код платформы сгорел из-за занятой почты")
	}
}

// Недействительный код отвечает раньше, чем проверяется почта: по ответу нельзя выяснить, занят ли адрес.
func TestInvalidCodeDoesNotRevealWhetherTheEmailIsTaken(t *testing.T) {
	f := newFixture(RegistrationInviteOnly, newMemberGate())
	f.repository.users["taken@example.com"] = domainUser("taken@example.com")
	if _, err := f.service.Register(context.Background(), registration("taken@example.com", "Z0000000000000000000000000000000000000000AA"), Client{}); !errors.Is(err, ErrInvitationInvalid) {
		t.Fatalf("занятая почта с неверным кодом = %v, want ErrInvitationInvalid", err)
	}
}

func TestMemberCodeCreatesTheUserAndAcceptsTheInvitation(t *testing.T) {
	f := newFixture(RegistrationInviteOnly, newMemberGate(memberCode))
	registered, err := f.service.Register(context.Background(), registration("staff@example.com", memberCode), Client{})
	if err != nil {
		t.Fatalf("регистрация по коду сотрудника: %v", err)
	}
	if f.gate.accepted[memberCode] != registered.User.ID || f.repository.users["staff@example.com"].OrganizationRights != 0 {
		t.Fatalf("приём кода: %+v, права пользователя %d", f.gate.accepted, f.repository.users["staff@example.com"].OrganizationRights)
	}
}

// Гонка: код проверили, а к приёму его уже потратили. Только что созданный пользователь убирается,
// иначе приглашённый без членства остался бы с аккаунтом, а повтор получил бы «почта занята».
func TestLostRaceOnAMemberCodeRemovesTheNewUser(t *testing.T) {
	gate := newMemberGate(memberCode)
	gate.acceptErr = ErrInvitationInvalid
	f := newFixture(RegistrationInviteOnly, gate)
	if _, err := f.service.Register(context.Background(), registration("staff@example.com", memberCode), Client{}); !errors.Is(err, ErrInvitationInvalid) {
		t.Fatalf("проигранная гонка = %v, want ErrInvitationInvalid", err)
	}
	if f.userCount() != 0 || len(f.repository.sessions) != 0 {
		t.Fatalf("осталось пользователей %d, сессий %d", f.userCount(), len(f.repository.sessions))
	}
}

// Исход приёма неизвестен: удалить пользователя значило бы потерять уже принятое членство.
func TestAmbiguousAcceptFailureKeepsTheUser(t *testing.T) {
	gate := newMemberGate(memberCode)
	gate.acceptErr = errors.New("соединение разорвано после фиксации")
	f := newFixture(RegistrationInviteOnly, gate)
	if _, err := f.service.Register(context.Background(), registration("staff@example.com", memberCode), Client{}); err == nil || errors.Is(err, ErrInvitationInvalid) {
		t.Fatalf("неоднозначный сбой = %v, want обычную ошибку", err)
	}
	if f.userCount() != 1 {
		t.Fatalf("пользователей %d, ожидался один: неоднозначный сбой не должен удалять учётную запись", f.userCount())
	}
}

func TestInfrastructureFailureWhileCheckingACodeIsNotAnInvalidCode(t *testing.T) {
	gate := newMemberGate(memberCode)
	gate.checkErr = errors.New("база недоступна")
	f := newFixture(RegistrationInviteOnly, gate)
	if _, err := f.service.Register(context.Background(), registration("staff@example.com", memberCode), Client{}); err == nil || errors.Is(err, ErrInvitationInvalid) {
		t.Fatalf("сбой проверки = %v: не должен выдаваться за недействительный код", err)
	}
}

func TestOpenRegistrationStillHonoursAProvidedCode(t *testing.T) {
	f := newFixture(RegistrationOpen, newMemberGate(memberCode))
	if _, err := f.service.Register(context.Background(), registration("plain@example.com", ""), Client{}); err != nil {
		t.Fatalf("открытая регистрация без кода: %v", err)
	}
	if f.repository.users["plain@example.com"].OrganizationRights != 0 {
		t.Fatal("регистрация без кода не должна давать право на организацию")
	}
	if _, err := f.service.Register(context.Background(), registration("typo@example.com", "Z0000000000000000000000000000000000000000AA"), Client{}); !errors.Is(err, ErrInvitationInvalid) {
		t.Fatalf("неверный код в открытом режиме = %v, want ErrInvitationInvalid", err)
	}
	registered, err := f.service.Register(context.Background(), registration("staff@example.com", memberCode), Client{})
	if err != nil || f.gate.accepted[memberCode] != registered.User.ID {
		t.Fatalf("код сотрудника в открытом режиме: %v, принято %+v", err, f.gate.accepted)
	}
}

// Предел регистраций с адреса считает и неверные коды: иначе подбор шёл бы без счёта.
func TestWrongCodesConsumeTheRegistrationRateLimit(t *testing.T) {
	f := newFixture(RegistrationInviteOnly, newMemberGate())
	client := Client{IPAddress: "198.51.100.7"}
	for attempt := 1; attempt <= registerIPLimit; attempt++ {
		if _, err := f.service.Register(context.Background(), registration("new@example.com", "Z0000000000000000000000000000000000000000AA"), client); !errors.Is(err, ErrInvitationInvalid) {
			t.Fatalf("попытка %d = %v", attempt, err)
		}
	}
	if _, err := f.service.Register(context.Background(), registration("new@example.com", platformCode), client); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("после исчерпания предела даже верный код должен ждать: %v", err)
	}
}

func TestModeFallsBackToClosed(t *testing.T) {
	if got := (Service{}).Mode(); got != RegistrationInviteOnly {
		t.Fatalf("Mode() = %q, want invite_only", got)
	}
	if got := (Service{}).WithRegistration(RegistrationOpen, nil).Mode(); got != RegistrationOpen {
		t.Fatalf("Mode() = %q, want open", got)
	}
}
