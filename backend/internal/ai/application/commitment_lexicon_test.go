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
	want := []lexiconToken{{"хотите", 0, 0}, {"да", 1, 1}, {"пришлю", 2, 2}, {"отвечу", 3, 3}, {"ок", 4, 4}}
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
	tokens := []lexiconToken{{"если", 0, 0}, {"нужно", 1, 1}, {"проверю", 1, 1}}
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
	split := []lexiconToken{{"отвечу", 0, 0}, {"может", 0, 0}, {"быть", 1, 1}}
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

func TestCommitmentRuledOutRecognisesOffersConditionsHedgesAndQuestions(t *testing.T) {
	for _, body := range []string{
		// слова обещания есть, но каждое исключено структурой
		"Хотите, пришлю прайс?", "Если хотите, перезвоню вам вечером.", "Если нужно, отправим расчёт.", "Давайте я перезвоню вам завтра.",
		"Возможно, ответим завтра.", "Обычно перезваниваем в тот же день, позвоню.", "Обращайтесь, если будут вопросы, отвечу.",
		"Если по результатам возникнут вопросы, позвоним вам.", "Не пришлю.", "Не отвечу и не пришлю.", "Хотите, пришлю? Или позвонить?",
		// слов обещания нет, а текст состоит из вопросов
		"Прислать вам прайс на почту?", "Отправить вам фото результата?", "Можно?", "На какой номер вам перезвонить?",
		"Здравствуйте! Позвонить вам?", "Добрый день, Анна! Подготовить для вас расчёт?", "Спасибо. Прислать вам прайс?",
		// пустое сообщение
		"", "   ", "\n",
	} {
		if !CommitmentRuledOut(body) {
			t.Errorf("a message without a possible promise must be ruled out: %q", body)
		}
	}
}

func TestCommitmentRuledOutKeepsEverythingThatMayStillBeAPromise(t *testing.T) {
	for _, body := range []string{
		// допущенное слово обещания
		"Пришлю завтра.", "Хотите прайс? Завтра позвоню вам.", "Возможно, есть скидка. Проверю и напишу.", "Не волнуйтесь, отправлю сегодня.",
		"Позвоню завтра. Вам удобно?",
		// слов обещания нет, но это не вопрос и не реплика: решает модель
		"Подготовили документы, можете забирать.", "Мы всегда отправляем чек на почту.", "Будет сделано.", "Будет сделано. Когда вам удобно?",
		"Хорошо, будет сделано. Вам удобно?", "Мы решим этот вопрос. Вам удобно?", "Здравствуйте, это сервис, мы получили заявку. Подождёте?",
		"Здравствуйте!", "Спасибо!", "Ок",
	} {
		if CommitmentRuledOut(body) {
			t.Errorf("this message may still carry a promise: %q", body)
		}
	}
}

func TestCommitmentSupportSeparatesAdmittedOpenAndRuledOut(t *testing.T) {
	for _, test := range []struct {
		body string
		want LexicalSupport
	}{
		{"Пришлю счёт завтра.", LexicalAdmitted},
		{"Позвоню завтра. Вам удобно?", LexicalAdmitted},
		{"Хотите, пришлю прайс?", LexicalRuledOut},
		{"Прислать вам прайс?", LexicalRuledOut},
		{"Возможно, ответим завтра.", LexicalRuledOut},
		{"Подготовили документы, можете забирать.", LexicalOpen},
		{"Будет сделано.", LexicalOpen},
		{"Здравствуйте!", LexicalOpen},
		{"", LexicalRuledOut},
	} {
		if got := CommitmentSupport(test.body); got != test.want {
			t.Errorf("%q: got %d, want %d", test.body, got, test.want)
		}
	}
	// Слово обещания сильнее исключения вопросом: вопрос со словом обещания остаётся допущенным, как у прежнего допуска.
	if CommitmentSupport("Вам удобно, если я позвоню завтра?") != LexicalAdmitted {
		t.Error("a question that still has an unexcluded promise word stays admitted")
	}
}

func TestLexiconSentencesKeepTheQuestionMarkAndNormalizeTheText(t *testing.T) {
	got := lexiconSentences("Добрый день, Анна! Отправить Ёлку? Да... Хорошо?! Нет.")
	want := []lexiconSentence{
		{words: []string{"добрый", "день", "анна"}},
		{words: []string{"отправить", "елку"}, question: true},
		{words: []string{"да"}},
		{words: []string{"хорошо"}, question: true},
		{words: []string{"нет"}},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d sentences %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if strings.Join(got[i].words, " ") != strings.Join(want[i].words, " ") || got[i].question != want[i].question {
			t.Errorf("sentence %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
	if len(lexiconSentences(" — ... ")) != 0 {
		t.Error("a dash and punctuation are not words")
	}
}

func TestAPoliteReplyIsTrivialOnlyWhenShortAndOnlyAtTheStart(t *testing.T) {
	// Четыре слова после приветствия — уже повествование: решает модель.
	if CommitmentRuledOut("Здравствуйте, мы получили вашу заявку. Позвонить вам?") {
		t.Error("a statement of four words is not a greeting")
	}
	if !CommitmentRuledOut("Добрый вечер, Анна! Позвонить вам завтра?") {
		t.Error("a short greeting does not change a question")
	}
	// Приветствие не в начале предложения не считается приветствием.
	if CommitmentRuledOut("Нужен вам, спасибо. Позвонить завтра?") {
		t.Error("a politeness word that does not start the sentence does not make a reply trivial")
	}
}

func TestCommitmentRuledOutClosesOffersOfServiceAndHedgesThatHaveNoPromiseWord(t *testing.T) {
	for _, body := range []string{
		// предложение услуги: компания не берёт обязательство
		"Могу уточнить у мастера, если вам это важно.", "Можем отправить расчёт.", "Готовы прислать договор.", "Рады сообщить, что заказ готов.",
		"Будем рады видеть вас.", "Готова подобрать вариант.",
		// смягчение в предложении без слова обещания
		"Возможно, получится отправить сегодня.", "Наверное, это получится сделать завтра.", "Вероятно, потребуется время.", "Может быть, получится.",
		// несколько закрытых предложений и приветствие
		"Здравствуйте! Могу уточнить у мастера. Вам это нужно?", "Добрый день. Возможно, получится отправить.",
	} {
		if !CommitmentRuledOut(body) {
			t.Errorf("a sentence without a promise word that offers or hedges must be closed: %q", body)
		}
	}
}

func TestCommitmentRuledOutLeavesOpenWhatTheModelHasToJudge(t *testing.T) {
	for _, body := range []string{
		"Подготовили документы, можете забирать.", "Мы всегда отправляем чек на почту.", "Договор мы направляем после предоплаты.",
		"Будет сделано в течение часа.", "Могу уточнить. Будет сделано.", "Возможно, получится. Подготовим акт завтра.",
		// модальное слово без инфинитива рядом не предложение услуги
		"Я рад вашему отзыву.", "Готов к встрече в понедельник.", "Можем в пятницу, а пока подготовим договор.",
	} {
		if CommitmentRuledOut(body) {
			t.Errorf("this message may still carry a promise: %q", body)
		}
	}
}

func TestOneHedgeListServesTheAnchorsAndTheSentencesWithoutThem(t *testing.T) {
	for _, hedge := range []string{"Возможно", "Наверное", "Вероятно", "Пожалуй", "Обычно"} {
		if HasCommitmentAnchor(hedge + ", ответим завтра.") {
			t.Errorf("«%s» softens the promise word in its sentence", hedge)
		}
		if !HasCommitmentAnchor(hedge + ", это хорошо. Ответим завтра.") {
			t.Errorf("«%s» in another sentence must not cancel the promise", hedge)
		}
	}
	for _, body := range []string{"Как правило, отвечаем быстро. Ответим завтра.", "Может быть, нет. Ответим завтра."} {
		if !HasCommitmentAnchor(body) {
			t.Errorf("a pair-word hedge belongs to its own sentence: %q", body)
		}
	}
	for _, word := range []string{"уточнить", "помочь", "отправить", "сделать", "привезти", "найти", "принести"} {
		if !isInfinitive(word) {
			t.Errorf("%q is an infinitive", word)
		}
	}
	for _, word := range []string{"мать", "есть", "пять", "дать", "ть", "ответ", "позвоню", "честь", "стоимость", "кровать", "новости", "области", "части", "возможности", "везти"} {
		if isInfinitive(word) {
			t.Errorf("%q is not an infinitive", word)
		}
	}
}

func TestNegationDoesNotReachAcrossASentenceBoundary(t *testing.T) {
	// Отказ в одном предложении не отменяет обещание в следующем.
	for _, body := range []string{"Нет. Ответим завтра.", "К сожалению, нет! Позвоним в пятницу.", "Не получится сегодня. Отправим завтра.", "Ничего. Сообщим позже."} {
		if !HasCommitmentAnchor(body) {
			t.Errorf("a promise after a refusal in another sentence stays a promise: %q", body)
		}
	}
	if HasCommitmentAnchor("Нет, не пришлю.") || HasCommitmentAnchor("Мы ничего не отправим.") {
		t.Error("a negation in the same sentence still cancels the word")
	}
}

func TestCommitmentOpenTextKeepsOnlyTheSentencesWherePromisesAreStillPossible(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		// вопрос и предложение услуги рядом с обычным ответом убираются, ответ остаётся как написан
		{"Да, в студии можно с животными. Скинуть вам фото интерьеров залов?", "Да, в студии можно с животными."},
		{"Стандартно два рабочих дня. Если сроки горят, есть срочная печать — могу рассчитать, если пришлёте макет.", "Стандартно два рабочих дня."},
		{"Добрый день! Готовых в витрине нет. Хотите, пришлю примеры?", "Готовых в витрине нет."},
		{"Хотите, пришлю прайс? Завтра позвоню вам.", "Завтра позвоню вам."},
		{"Мастер заедет завтра. Хотите, пришлю? Курьер привезёт в пятницу.", "Мастер заедет завтра. Курьер привезёт в пятницу."},
		// слово обещания остаётся с регистром и знаками
		{"Спасибо! Пришлём Вам договор в ПЯТНИЦУ, ок?", "Пришлём Вам договор в ПЯТНИЦУ, ок?"},
		// нечего оставлять — возвращается весь текст
		{"Здравствуйте!", "Здравствуйте!"},
		{"Хотите, пришлю прайс?", "Хотите, пришлю прайс?"},
		{"", ""},
	} {
		if got := CommitmentOpenText(tc.body); got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.body, got, tc.want)
		}
	}
}

func TestCommitmentStatedNeedsAPromiseWordInAStatement(t *testing.T) {
	for body, want := range map[string]bool{
		"Позвоню завтра.":                          true,
		"Здравствуйте! Пришлю расчёт до вечера.":   true,
		"Позвоню завтра, ок?":                      false, // вопрос не считается: решать его оставлено модели
		"Будет ли удобно, если тренер перезвонит?": false,
		"Удобно? Перезвоню вам в пять.":            true,
		"Хотите, пришлю?":                          false,
		"Возможно, ответим.":                       false,
		"Не пришлю.":                               false,
		"Будет сделано.":                           false, // слова обещания нет
		"":                                         false,
	} {
		if got := CommitmentStated(body); got != want {
			t.Errorf("%q: got %v, want %v", body, got, want)
		}
	}
}

func TestLexiconSentencesKeepTheOriginalTextOfEachSentence(t *testing.T) {
	got := lexiconSentences("  Добрый день, Анна!  Пришлём ВАМ счёт… Ёлка?  ")
	texts := []string{}
	for _, sentence := range got {
		texts = append(texts, sentence.text)
	}
	if strings.Join(texts, "|") != "Добрый день, Анна!|Пришлём ВАМ счёт…|Ёлка?" {
		t.Fatalf("texts: %q", texts)
	}
}

func TestMoreModalWordsMakeAnOfferNotAPromise(t *testing.T) {
	for _, body := range []string{"Юрист сможет изучить материалы на этой неделе.", "Мастер сможет приехать завтра.", "Можно записать вас на пятницу.", "Смогу уточнить у мастера."} {
		if !CommitmentRuledOut(body) {
			t.Errorf("a possibility is not a promise: %q", body)
		}
	}
}

func TestOnlyTheFirstPersonVerbToPromiseIsAPromiseWord(t *testing.T) {
	for _, body := range []string{"Обещаю ответить сегодня.", "Обещаем вернуть деньги до пятницы.", "Пообещаю, что позвонят.", "ОБЕЩАЕМ, что перезвоним."} {
		if !HasCommitmentAnchor(body) {
			t.Errorf("a first-person promise word must be admitted: %q", body)
		}
	}
	for _, body := range []string{
		"Других обещаний в переписке не было.", "Поставщик обещал на этой неделе, но точно сказать не могу.", "Это обещание мы выполнили.",
		"Менеджер обещала позвонить вчера.", "Обещание данное ранее остаётся в силе.", "Они обещают, но сроки сдвигают.",
	} {
		if HasCommitmentAnchor(body) {
			t.Errorf("a noun, a past form or somebody else's promise is not a new promise of the company: %q", body)
		}
	}
	if HasCommitmentAnchor("Ничего не обещаю.") || HasCommitmentAnchor("Не обещаем сроки.") {
		t.Error("a negated promise verb is not a promise")
	}
}

func TestAMarkerInALaterClauseClosesOnlyItsOwnClauseAndAnOfferOfTheCompanyClosesTheSentence(t *testing.T) {
	// Обещание не пропадает из текста для прохода из-за слова смягчения или «можно» в следующей фразе.
	kept := []string{
		"Вижу отмену. Деньги вернём на карту сегодня, обычно банк зачисляет их в течение 1–3 дней.",
		"Понятно. В субботу утром сборщик отрегулирует петли, а если понадобится, поставит новые.",
		"Видим, это наш брак. В течение двух часов курьер доставит новый букет, этот можно не возвращать.",
		"Юрист изучит документы и до пятницы даст заключение: какие есть риски и что можно ответить арендодателю.",
	}
	for _, body := range kept {
		if CommitmentRuledOut(body) {
			t.Errorf("a marker in a later clause must not rule out the message: %q", body)
		}
		open := CommitmentOpenText(body)
		if want := strings.TrimSpace(body[strings.LastIndex(body[:len(body)-1], ". ")+1:]); !strings.Contains(open, want) {
			t.Errorf("the sentence with the promise must stay in the text for the scan: got %q, want it to contain %q", open, want)
		}
	}
	// Признак в ведущей фразе по-прежнему закрывает предложение, в том числе за вежливым словом.
	closed := []string{
		"Возможно, ответим завтра.",
		"Конечно, возможно, получится отправить сегодня.",
		"Спасибо, обычно отвечаем в тот же день.",
		"Хотите, подберём варианты.",
		"Если нужно, подберём варианты.",
		// предложение услуги от первого лица закрывает предложение целиком, где бы оно ни стояло
		"Если сроки горят, есть срочная печать — могу рассчитать, если пришлёте макет.",
		"Мастер свободен в пятницу, а если вам удобнее, могу записать на понедельник.",
		"Если передадите дело нам, юрист сможет изучить материалы уже на этой неделе.",
		"Курьеры работают до 22:00, а если нужно позже, смогут привезти завтра.",
	}
	for _, body := range closed {
		if !CommitmentRuledOut(body) {
			t.Errorf("a marker in the leading clause rules the sentence out: %q", body)
		}
	}
}

func TestClauseBreaksFollowCommasColonsAndDashesAndSkipEmptyOnes(t *testing.T) {
	sentences := lexiconSentences("Спасибо,, вернём — обычно за три дня: ждите")
	if len(sentences) != 1 {
		t.Fatalf("one sentence expected: %+v", sentences)
	}
	s := sentences[0]
	if got := strings.Join(s.words, " "); got != "спасибо вернем обычно за три дня ждите" {
		t.Fatalf("words: %q", got)
	}
	if len(s.breaks) != 3 || s.breaks[0] != 1 || s.breaks[1] != 2 || s.breaks[2] != 6 {
		t.Fatalf("clause breaks: %v", s.breaks)
	}
	// ведущая фраза — первая содержательная: «спасибо» только вежливое слово
	if s.leadEnd() != 2 {
		t.Fatalf("the leading clause is «вернём»: %d", s.leadEnd())
	}
	if lexiconSentences("Да.")[0].leadEnd() != 1 {
		t.Fatal("a sentence of lead words is led by all of it")
	}
}

func TestLexiconFunctionsAgreeAndNeverPanicOnPunctuationHeavyText(t *testing.T) {
	words := []string{"ответим", "завтра", "возможно", "обычно", "могу", "сможет", "хотите", "если", "нужно", "можно", "не", "банк", "зачисляет", "деньги", "вернём",
		"спасибо", "конечно", "мастер", "приедет", "а", "и", "как", "правило", "пришлю", "будет", "готово", "?", ",", ",", ":", "—", ".", "!", ";", "\n", " "}
	rng := newLexiconRandom(1)
	for i := 0; i < 3000; i++ {
		var body strings.Builder
		for n := rng.next(14); n > 0; n-- {
			body.WriteString(words[rng.next(len(words))])
			body.WriteByte(' ')
		}
		text := body.String()
		support := CommitmentSupport(text)
		if HasCommitmentAnchor(text) != (support == LexicalAdmitted) {
			t.Fatalf("the anchor and the admitted state must agree: %q", text)
		}
		if (support == LexicalRuledOut) != CommitmentRuledOut(text) && support != LexicalAdmitted {
			t.Fatalf("ruled out must agree with the support: %q", text)
		}
		if open := CommitmentOpenText(text); strings.TrimSpace(text) != "" && open == "" {
			t.Fatalf("the open text of a non-empty message is never empty: %q", text)
		}
		_ = CommitmentStated(text)
	}
}

// lexiconRandom — детерминированный генератор для проверки без зависимости от math/rand.
type lexiconRandom struct{ state uint64 }

func newLexiconRandom(seed uint64) *lexiconRandom {
	return &lexiconRandom{state: seed*2862933555777941757 + 3037000493}
}

func (r *lexiconRandom) next(n int) int {
	r.state = r.state*6364136223846793005 + 1442695040888963407
	return int((r.state >> 33) % uint64(n))
}
