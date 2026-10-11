package application

import (
	"strings"
	"testing"
)

func TestCommitmentAnchorAcceptsFuturePromisesOfTheCompany(t *testing.T) {
	for _, body := range []string{
		// прежний словарь: ничего из него не потеряно
		"Проверю и отвечу вам сегодня.", "Отправим договор завтра.", "Пришлём расчёт.", "Пришлем расчет.", "Уточню у мастера и напишу.",
		"Свяжемся с вами в понедельник.", "Передам ваш вопрос.", "Сообщим результат.", "Скину примеры.", "Вышлю счёт.",
		"Подготовлю смету.", "Сделаю перерасчёт.", "Позвоню через час.", "Перезвоню вам.", "Обещаю ответить сегодня.",
		"Мы вам обязательно сообщим.", "I'll check and reply.", "We will send the invoice.", "We'll confirm by email.",
		// первое лицо, новые глаголы
		"Узнаю на складе.", "Выясню у мастера.", "Согласую с руководителем.", "Оформлю возврат.", "Рассчитаем по площади.",
		"Подберём вариант.", "Забронирую номер.", "Закажу колодки.", "Доставим завтра.", "Вернём деньги.", "Верну деньги.",
		"Займусь вашим вопросом.", "Разберёмся в причине.", "Напомним о визите.", "Дам знать, как появится.", "Вернусь с ответом.",
		"Возьму ваше дело в работу.", "Возьмём заявку в работу.", "Дадим знать позже.",
		// третье лицо: сотрудник компании обещает ответ
		"Менеджер перезвонит вам в течение часа.", "С вами свяжется наш администратор.", "Вам ответит юрист.", "Бухгалтер вышлет акт.",
		"Куратор отправит программу.", "Диспетчер сообщит время.", "Врач позвонит после приёма.", "Вам пришлют договор.",
		// безличная форма
		"Договор будет отправлен вам сегодня.", "Ответ будет дан в течение двух дней.", "Вам будет выслано подтверждение.", "Документы будут подготовлены к пятнице.",
		"Договор будет вам отправлен сегодня.", "Счёт будет сегодня выставлен.", "Заказ будет готов завтра.", "Документы будут готовы к пятнице.",
		"Привезём заказ завтра.", "Возместим ущерб.", "Назначу вам время консультации.", "Закреплю за вами менеджера.", "Сверю данные и вернусь.",
		"Сброшу вам реквизиты.", "Отзвонюсь через десять минут.", "Выдадим справку сегодня.",
		"Посмотрим расписание и ответим.", "Посмотрю, есть ли свободные окна.",
		"Дам ответ завтра.", "Дадим обратную связь до вечера.", "Специалист вернётся к вам с ответом.", "Юристы вернутся к вам завтра.",
		// разговорные формы
		"Щас гляну и отпишусь.", "Кину ссылку через минуту.", "Наберу вас через пять минут.",
		// регистр и знаки
		"ПРИШЛЮ СЧЁТ!", "пришлю, счёт", "Отвечу…",
	} {
		if !HasCommitmentAnchor(body) {
			t.Errorf("a promise of the company must be allowed: %q", body)
		}
	}
}

func TestCommitmentAnchorRejectsWhatIsNotAPromiseOfTheCompany(t *testing.T) {
	for _, body := range []string{
		// просьбы клиенту и инфинитивы: слова похожи, обещания нет
		"Пришлите, пожалуйста, фото.", "Свяжитесь с нами по телефону.", "Позвоните администратору.", "Напишите нам в WhatsApp.",
		"На какой номер вам перезвонить?", "Прислать вам прайс?", "Могу отправить расчёт.", "Отправить вам фото результата?",
		// прошедшее время и подстроки глаголов
		"Ссылку на оплату я уже скинула выше.", "Перезвонила, но телефон был недоступен.", "Отправил реквизиты выше.", "Выслала вам счёт.",
		"Я отправлюсь на склад после обеда.", "Мы ответили вам вчера.", "Уточнил у мастера: деталь есть.",
		// отрицание прямо перед глаголом
		"Не пришлю, у нас нет такой модели.", "Не пришлём вам договор.", "Ничего не обещаю.", "Не могу ничего обещать по срокам.",
		"Мы не будем отвечать на такие вопросы.", "Перезванивать не будем, пишите в чат.", "Нет, не отвечу.",
		"Договор не будет отправлен.", "Ответ не будет дан.",
		// глаголы услуги в третьем лице описывают визит, а не обещание в переписке
		"Мастер сделает окрашивание за два часа.", "Врач подберёт лечение на приёме.", "Механик осмотрит подвеску при приёме.",
		"Преподаватель составит программу на первом занятии.",
		// справка и общие слова
		"Процедура займёт около часа.", "Мы работаем с десяти до восьми.", "Результат будет заметен через неделю.", "Обычно отвечаем в течение часа.",
		"Посмотрим, что можно сделать.", "Посмотрим по графику.", "Решим по результатам поездки.", "Решим по ситуации.",
		"Здравствуйте! Чем можем помочь?", "", "   ", "goodwill", "I will", "will", "Вернётся из отпуска в понедельник.", "Дам вам совет.",
		// чужие слова с общей основой
		"Свободно в субботу.", "Курьер сейчас в пути.", "Менеджеры отвечают в рабочие часы.", "Счёт высылается автоматически.",
	} {
		if HasCommitmentAnchor(body) {
			t.Errorf("this is not a promise of the company: %q", body)
		}
	}
}

func TestCommitmentAnchorChecksNegationOnlyRightBeforeTheVerb(t *testing.T) {
	// Отрицание в другой части фразы не отменяет обещание рядом.
	for _, body := range []string{
		"Не волнуйтесь, отправлю сегодня.", "Ответа сегодня нет, но завтра обязательно отвечу.", "Это не сложно, подготовлю и пришлю.",
		"Я не отвечу сегодня, но завтра пришлю расчёт.",
	} {
		if !HasCommitmentAnchor(body) {
			t.Errorf("a distant negation must not cancel the promise: %q", body)
		}
	}
	// Отрицание сразу перед словом снимает именно это слово, но не остальные.
	if !HasCommitmentAnchor("Не отвечу сегодня, пришлю завтра.") || HasCommitmentAnchor("Не отвечу и не пришлю.") {
		t.Fatal("negation must apply to the word that follows it, not to the whole message")
	}
}

func TestCommitmentAnchorSupportsThePhraseWithWordsInside(t *testing.T) {
	for body, want := range map[string]bool{
		"Возьму в работу.": true, "Возьму ваше дело в работу.": true, "Возьмём вашу заявку и в работу.": true,
		"Возьму ваше сложное дело срочно в работу.": false, "Возьму трубку.": false, "В работу не возьму.": false,
	} {
		if got := HasCommitmentAnchor(body); got != want {
			t.Errorf("HasCommitmentAnchor(%q) = %v, want %v", body, got, want)
		}
	}
}

func TestCommitmentAnchorEnglishNeedsAFollowingWord(t *testing.T) {
	for body, want := range map[string]bool{
		"I will check.": true, "We will call you.": true, "It will": false, "We’ll send it.": true, "Goodwill is great": false,
	} {
		if got := HasCommitmentAnchor(body); got != want {
			t.Errorf("HasCommitmentAnchor(%q) = %v, want %v", body, got, want)
		}
	}
}

func TestCommitmentLexiconTablesAreConsistent(t *testing.T) {
	seen := map[string]string{}
	for name, words := range map[string][]string{"first person": commitmentFirstPerson, "third person": commitmentThirdPerson, "participles": commitmentParticiples} {
		for _, word := range words {
			if word != strings.ToLower(word) || strings.ContainsAny(word, "ёЁ ") || word == "" {
				t.Errorf("%s: %q must be a lower-case word without spaces or the letter ё", name, word)
			}
			if previous, duplicated := seen[word]; duplicated {
				t.Errorf("%q is listed twice: %s and %s", word, previous, name)
			}
			seen[word] = name
		}
	}
	for _, word := range commitmentFirstPerson {
		if !HasCommitmentAnchor("Я " + word + " вам") {
			t.Errorf("a listed first-person form must be allowed: %q", word)
		}
	}
	for _, word := range commitmentThirdPerson {
		if !HasCommitmentAnchor("Менеджер " + word + " вам") {
			t.Errorf("a listed third-person form must be allowed: %q", word)
		}
		if HasCommitmentAnchor("Менеджер не " + word + " вам") {
			t.Errorf("a negated form must not be allowed: %q", word)
		}
	}
	for stem := range commitmentParticipleOf {
		if !HasCommitmentAnchor("Договор будет " + stem) {
			t.Errorf("a listed participle must be allowed: %q", stem)
		}
	}
}

func TestExplicitAgreementTriggerForCommitmentUsesTheSharedLexicon(t *testing.T) {
	for _, body := range []string{"Менеджер перезвонит.", "Пришлю договор.", "Не пришлю договор.", "Пришлите договор.", "Договор будет отправлен."} {
		if got, want := explicitAgreementTrigger("COMMITMENT", body), HasCommitmentAnchor(body); got != want {
			t.Errorf("the validator and the shared lexicon disagree on %q: %v and %v", body, got, want)
		}
	}
}

func TestPossibleBusinessCommitmentEvidenceNeedsAnOutgoingPromiseWithoutHedging(t *testing.T) {
	for _, tc := range []struct {
		direction, body string
		want            bool
	}{
		{"OUTGOING", "Менеджер перезвонит вам в течение часа.", true},
		{"INCOMING", "Я пришлю документы вечером.", false},
		{"OUTGOING", "Возможно, ответим завтра.", false},
		{"OUTGOING", "Может быть, перезвоним позже.", false},
		{"OUTGOING", "Не пришлю.", false},
		{"OUTGOING", "Пришлите фото.", false},
	} {
		if got := PossibleBusinessCommitmentEvidence(ContextMessage{ID: "m1", Direction: tc.direction, Body: tc.body}); got != tc.want {
			t.Errorf("PossibleBusinessCommitmentEvidence(%s, %q) = %v, want %v", tc.direction, tc.body, got, tc.want)
		}
	}
}

func TestCommitmentAnchorIgnoresOffersAndConditionsRightBeforeTheVerb(t *testing.T) {
	for _, body := range []string{
		"Если хотите, перезвоню вам вечером.", "Хотите, пришлю примеры работ?", "Хотите, я свяжусь с вами завтра?",
		"Если понадобится, сообщим дополнительно.", "Хотите, подготовлю для вас подборку?", "При необходимости перезвоним.",
		"Давайте я вам напишу позже.", "Если нужно, отправлю договор.", "Если вы захотите, мы напомним о визите.", "По запросу вышлем акт.",
		"Если надо, проверю наличие.",
	} {
		if HasCommitmentAnchor(body) {
			t.Errorf("an offer is not a promise: %q", body)
		}
	}
	for _, body := range []string{
		// предложение в другой фразе не отменяет обещание
		"Хотите узнать цену? Уточню у мастера.", "Хотите?\nПришлю.", "Хотите, пришлю? Хорошо. Пришлю сегодня.",
		// маркер дальше трёх слов или не тот
		"Как только понадобится помощь, мы сразу ответим.", "Если вам удобно, отправлю договор сегодня.", "Спасибо, что захотели с нами работать. Отвечу завтра.",
		"Если будут вопросы — позвоню.", "Давайте не будем спорить, я проверю сегодня.",
	} {
		if !HasCommitmentAnchor(body) {
			t.Errorf("a promise next to an offer must stay allowed: %q", body)
		}
	}
}

func TestADashBetweenSpacesIsNotAWordAndDoesNotMoveTheNegationWindow(t *testing.T) {
	for body, want := range map[string]bool{"Не - пришлю.": false, "Не - обещаю - ничего.": false, "Хотите - пришлю.": false, "Мы - позвоним - завтра.": true, "Всё ' - ' отвечу.": true} {
		if got := HasCommitmentAnchor(body); got != want {
			t.Errorf("HasCommitmentAnchor(%q) = %v, want %v", body, got, want)
		}
	}
}

func TestLexiconTokensKeepTrackOfSentences(t *testing.T) {
	tokens := lexiconTokens("Хотите? Да\nПришлю; Отвечу… Ок")
	want := []lexiconToken{{"хотите", 0}, {"да", 1}, {"пришлю", 2}, {"отвечу", 3}, {"ок", 4}}
	if len(tokens) != len(want) {
		t.Fatalf("tokens = %+v, want %+v", tokens, want)
	}
	for i := range want {
		if tokens[i] != want[i] {
			t.Fatalf("token %d = %+v, want %+v", i, tokens[i], want[i])
		}
	}
}

func TestOfferPairsDoNotReachAcrossASentenceBoundary(t *testing.T) {
	tokens := []lexiconToken{{"если", 0}, {"нужно", 1}, {"проверю", 1}}
	if commitmentOfferedAt(tokens, 2) {
		t.Fatal("a pair split by a sentence boundary is not an offer marker")
	}
	tokens[0].sentence = 1
	if !commitmentOfferedAt(tokens, 2) {
		t.Fatal("the same pair inside one sentence is an offer marker")
	}
}

func TestAHedgeSoftensOnlyItsOwnSentence(t *testing.T) {
	for body, want := range map[string]bool{
		"Возможно, ответим завтра.":                                  false,
		"Ответим завтра, возможно.":                                  false,
		"Может быть, перезвоним позже.":                              false,
		"Мы можем, может быть, прислать позже.":                      false,
		"Возможно, у нас есть скидка. Проверю и напишу вам.":         true,
		"Проверю и напишу вам. Возможно, скидка будет.":              true,
		"Скидка возможна. Отвечу сегодня.":                           true,
		"Может быть несколько причин. Выясню и позвоню вам.":         true,
		"Причин может быть несколько, после осмотра выясню причину.": false,
		"Может ли это быть? Проверю.":                                true,
		"Если клиент может подтвердить, мы отправим договор.":        true,
	} {
		if got := HasCommitmentAnchor(body); got != want {
			t.Errorf("HasCommitmentAnchor(%q) = %v, want %v", body, got, want)
		}
	}
}

func TestTheWordsOfAHedgeMustStandInOneSentence(t *testing.T) {
	split := []lexiconToken{{"отвечу", 0}, {"может", 0}, {"быть", 1}}
	if commitmentHedgedAt(split, 0) {
		t.Fatal("«может» and «быть» in different sentences are not a hedge")
	}
	split[2].sentence = 0
	if !commitmentHedgedAt(split, 0) {
		t.Fatal("«может быть» in the sentence of the verb is a hedge")
	}
}

func TestAStandingOfferThatDependsOnTheCustomerIsNotAPromise(t *testing.T) {
	for _, body := range []string{
		"Если появятся вопросы, мы с радостью ответим.", "Если вам что-то понадобится, обязательно ответим.", "Если не дозвонитесь, пишите, я отвечу.",
		"По любым вопросам обращайтесь, ответим.", "Если по результатам возникнут вопросы, позвоним вам и всё объясним.", "Появятся вопросы — сразу отвечу.",
		"Если захочется уточнить детали, перезвоню.", "Звоните, если что, перезвоню при первой возможности.", "Напишите нам, и мы вам ответим.",
		"Если получится, отвечу сегодня.", "Если возникнет необходимость, позвоним.", "Возникнут сложности — сразу позвоним.",
	} {
		if HasCommitmentAnchor(body) {
			t.Errorf("a standing offer is not a promise: %q", body)
		}
	}
	for _, body := range []string{
		// срок, а не условие
		"Как только появится информация, пришлём вам письмо.", "Как только появятся результаты, позвоним вам.", "Когда появятся результаты, мы позвоним.",
		"После того как возникнут вопросы у юриста, он вам ответит.", "Пока не появятся вопросы, не звоним. Завтра пришлю договор.",
		// вежливая форма обещания без события в будущем
		"Если вам удобно, отправлю договор сегодня.", "Если не возражаете, перезвоню завтра.", "Если вы не против, пришлю на почту.",
		// приглашение в другой фразе
		"Обращайтесь в любое время. Завтра перезвоню и всё расскажу.", "Пишите, если что. Пришлю расчёт сегодня.",
		// сам по себе глагол события не делает обещание условным
		"Проверю, появились ли вопросы у юриста, и отвечу.",
	} {
		if !HasCommitmentAnchor(body) {
			t.Errorf("a promise with a deadline or a polite form must stay allowed: %q", body)
		}
	}
}

func TestAHabitualPhraseSoftensOnlyItsOwnSentence(t *testing.T) {
	for body, want := range map[string]bool{
		"Обычно перезвоним в течение часа.":                         false,
		"Как правило, ответим в тот же день.":                       false,
		"Обычно отвечаем за час. Сегодня отвечу вам лично.":         true,
		"Как правило, мы быстро отвечаем. Сейчас проверю и напишу.": true,
		"Как мы и договаривались, пришлю договор сегодня.":          true,
		"Правило такое: отвечу в течение дня.":                      true,
	} {
		if got := HasCommitmentAnchor(body); got != want {
			t.Errorf("HasCommitmentAnchor(%q) = %v, want %v", body, got, want)
		}
	}
}
