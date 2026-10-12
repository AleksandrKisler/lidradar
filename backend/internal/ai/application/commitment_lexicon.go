package application

import (
	"strings"
	"unicode"
)

// Словарь обещаний компании — единственный источник для всех мест, где решается, может ли сообщение
// быть обещанием: грамматика генерации фактов и договорённостей, подбор примеров, серверная проверка
// ответа и достройка факта в поставщике. Раньше слова жили в двух списках (проверка и генерация) и
// расходились: сервер принимал «передам», «сообщу», «напишу», а грамматика их не разрешала, и наоборот,
// корень «свяж» в грамматике пропускал «свяжитесь с нами» (просьбу клиенту), которую проверка отвергала.
//
// Словарь решает только, МОЖЕТ ли сообщение быть обещанием (допуск). Считать его обещанием или нет,
// по-прежнему решает модель по смыслу разговора; допуск ничего не навязывает. Поэтому в словарь входят
// слова будущего действия компании, а не любые глаголы:
//   - первое лицо единственного и множественного числа («пришлю», «пришлём») — глаголы действия и связи;
//   - третье лицо («менеджер перезвонит», «свяжется юрист») — только глаголы связи и проверки: с ними
//     сотрудник компании обещает ответ клиенту. Глаголы услуги («мастер сделает», «врач подберёт»)
//     описывают визит, а не обещание в переписке, и в третьем лице не допускаются;
//   - безличная форма «будет отправлен», «будет дан ответ», «заказ будет готов»;
//   - устойчивые обороты («дам знать», «возьму в работу») и глагол «обещать» в первом лице («обещаю», «обещаем»);
//   - английские «I'll», «we'll», «will».
//
// Совпадение идёт по целым словам, а не по подстрокам: «скинула», «перезвонила», «отправлюсь» и инфинитивы
// («перезвонить») обещаниями не считаются. Слово перестаёт быть допуском, если:
//   - оно стоит сразу после отрицания в том же предложении («не пришлю», «ничего не обещаю»; «Нет. Ответим завтра.» — обещание);
//   - перед ним в пределах трёх слов той же фразы предложение или условие («хотите, пришлю», «если нужно, отправим»);
//   - в том же предложении до слова включительно есть слово смягчения — «возможно», «наверное», «вероятно», «пожалуй», «обычно»,
//     «может быть», «как правило»: смягчение относится к своему предложению, а не ко всему сообщению, и действует вперёд — слово
//     смягчения в следующей фразе («Деньги вернём сегодня, обычно банк зачисляет за три дня») относится к ней, если только эта
//     фраза не состоит из одного смягчения («Ответим завтра, возможно»);
//   - обещание зависит от будущего обращения клиента («если появятся вопросы, ответим», «обращайтесь, отвечу»).
//
// Второй набор функций (ADR 0055) работает с предложениями и словом обещания не требует: CommitmentRuledOut сообщает, что обещания в
// сообщении быть не может (вопрос, предложение услуги «могу уточнить», смягчённая фраза, исключённое слово обещания; признак
// закрывает предложение, только если стоит в его ведущей фразе, а предложение услуги от первого лица закрывает его целиком),
// CommitmentSupport относит сообщение к допущенным словарём, открытым и исключённым, CommitmentStated требует слово обещания в
// повествовательном предложении, CommitmentOpenText оставляет в сообщении только предложения, где обещание возможно. Они задают границу, за
// которую не выходит смысловое решение проверочного прохода по обещаниям, и не заменяют его.
//
// Любое изменение словаря — изменение tuple: оно требует нового выбора до контрольной выборки и нового
// прогона на независимой выборке обязательств (ADR 0054, ADR 0055).

// commitmentFirstPerson — формы первого лица будущего времени, единственное и множественное число.
var commitmentFirstPerson = []string{
	// связь и ответ
	"отвечу", "ответим", "напишу", "напишем", "позвоню", "позвоним", "перезвоню", "перезвоним",
	"свяжусь", "свяжемся", "свяжу", "сообщу", "сообщим", "уведомлю", "уведомим", "оповещу", "оповестим",
	"предупрежу", "предупредим", "подтвержу", "подтвердим", "напомню", "напомним", "проинформирую", "проинформируем",
	"вернусь", "вернемся", "обязуюсь", "обязуемся",
	// проверка и уточнение
	"проверю", "проверим", "уточню", "уточним", "узнаю", "узнаем", "выясню", "выясним", "посмотрю",
	"спрошу", "спросим", "запрошу", "запросим", "поговорю", "посоветуюсь", "посоветуемся", "разберусь", "разберемся",
	"отслежу", "отследим", "рассмотрю", "рассмотрим", "согласую", "согласуем",
	// отправка и подготовка
	"отправлю", "отправим", "пришлю", "пришлем", "вышлю", "вышлем", "перешлю", "перешлем", "направлю", "направим",
	"передам", "передадим", "скину", "скинем", "загружу", "загрузим", "выставлю", "выставим", "выпишу", "выпишем",
	"подготовлю", "подготовим", "оформлю", "оформим", "составлю", "составим", "сформирую", "сформируем",
	"рассчитаю", "рассчитаем", "посчитаю", "посчитаем", "пересчитаю", "пересчитаем", "предоставлю", "предоставим",
	// действие
	"сделаю", "сделаем", "подберу", "подберем", "организую", "организуем", "забронирую", "забронируем",
	"зарезервирую", "зарезервируем", "закажу", "закажем", "доставлю", "доставим", "верну", "вернем",
	"исправлю", "исправим", "займусь", "займемся", "решу", "найду", "найдем", "внесу", "внесем",
	"поставлю", "поставим", "обработаю", "обработаем", "пробью", "пробьем",
	// доставка, возмещение, администрирование
	"привезу", "привезем", "приготовлю", "приготовим", "соберу", "соберем", "упакую", "упакуем", "отгружу", "отгрузим",
	"возмещу", "возместим", "компенсирую", "компенсируем", "пересмотрю", "пересмотрим", "закончу", "закончим", "завершу", "завершим",
	"сдадим", "выполню", "выполним", "зарегистрирую", "зарегистрируем", "закреплю", "закрепим", "назначу", "назначим",
	"запланирую", "запланируем", "активирую", "активируем", "подключу", "подключим", "настрою", "настроим", "выдам", "выдадим",
	"подпишу", "подпишем", "изучу", "изучим", "сверю", "сверим", "поинтересуюсь", "поинтересуемся", "проконсультируюсь", "проконсультируемся",
	"известим", "извещу",
	// разговорные формы
	"отпишусь", "отпишу", "отпишем", "гляну", "глянем", "кину", "кинем", "наберу", "наберем", "брошу", "сброшу", "сбросим",
	"отзвонюсь", "отзвонимся", "перекину", "перекинем", "закину", "закинем",
}

// Намеренно нет «посмотрим» и «решим» (множественное число): без другого слова обещания они почти всегда уклончивы —
// «посмотрим, что можно сделать», «решим по ситуации». «Посмотрим расписание и ответим» допускается словом «ответим».

// commitmentPromiseVerbs — глагол «обещать» в первом лице: «обещаю», «обещаем». Слова с тем же корнем в другой роли обещанием не бывают:
// «Других обещаний не было» (существительное), «Поставщик обещал на этой неделе» (чужое прошедшее обещание).
var commitmentPromiseVerbs = wordSet("обещаю", "обещаем", "пообещаю", "пообещаем", "обещаемся")

// commitmentThirdPerson — формы третьего лица будущего времени для глаголов связи и проверки: «менеджер
// перезвонит», «вам ответят». Глаголы услуги сюда не входят.
var commitmentThirdPerson = []string{
	"ответит", "ответят", "напишет", "напишут", "позвонит", "позвонят", "перезвонит", "перезвонят",
	"свяжется", "свяжутся", "сообщит", "сообщат", "уведомит", "уведомят", "подтвердит", "подтвердят",
	"напомнит", "напомнят", "проверит", "проверят", "уточнит", "уточнят",
	"отправит", "отправят", "пришлет", "пришлют", "вышлет", "вышлют", "направит", "направят",
	"передаст", "передадут",
}

// commitmentPhrases — устойчивые обороты из нескольких слов подряд.
var commitmentPhrases = [][]string{
	{"дам", "знать"}, {"дадим", "знать"}, {"даст", "знать"},
	{"дам", "ответ"}, {"дадим", "ответ"}, {"даст", "ответ"},
	{"дам", "обратную", "связь"}, {"дадим", "обратную", "связь"}, {"даст", "обратную", "связь"},
	{"вернется", "к", "вам"}, {"вернутся", "к", "вам"},
}

// commitmentTakeWords — «возьму ваше дело в работу»: между глаголом и «в работу» допускаются до трёх слов.
var commitmentTakeWords = wordSet("возьму", "возьмем")

// commitmentParticiples — страдательные причастия безличного обещания: «будет отправлен», «ответ будет дан».
var commitmentParticiples = []string{
	"отправлен", "направлен", "подготовлен", "передан", "выслан", "выставлен", "сообщен", "оформлен",
	"рассмотрен", "согласован", "подтвержден", "проверен", "дан", "предоставлен", "составлен", "сформирован",
	"исправлен", "организован", "доставлен", "возвращен", "пересчитан", "обработан", "переслан", "готов",
}

// commitmentHedgeWords — слова смягчения в предложении без слова обещания: «Возможно, получится отправить сегодня».
var commitmentHedgeWords = wordSet("возможно", "наверное", "вероятно", "пожалуй", "обычно")

// commitmentModalOffers — безличное «можно» с инфинитивом: «забрать можно с 12:00», «этот можно не возвращать». Оно описывает возможность
// и закрывает только свою фразу.
var commitmentModalOffers = wordSet("можно")

// commitmentSpeakerOffers — предложение услуги от лица компании («могу уточнить», «готовы отправить», «юрист сможет изучить»): такое слово
// делает предложением всё предложение, а не только свою фразу.
var commitmentSpeakerOffers = wordSet("могу", "можем", "сможем", "смогу", "сможет", "смогут", "готов", "готова", "готовы", "рад", "рада", "рады")

// commitmentNegators — слово перед глаголом, после которого это уже не обещание: «не пришлю», «ничего не обещаю».
var commitmentNegators = wordSet("не", "нет", "ни", "ничего", "никогда", "нельзя")

// Обещание, которое зависит от будущего обращения клиента, — «стоящее предложение», а не обещание: «если появятся вопросы,
// ответим», «обращайтесь, отвечу», «появятся вопросы — сразу позвоню». Ответ компании в таком обороте наступает только после
// действия клиента, и никакого срока выполнения у него нет. Признаки в той же фразе перед глаголом:
//   - «если» и после него слово о будущем событии со стороны клиента (commitmentContingentWords);
//   - приглашение клиенту обратиться (commitmentInvitationWords);
//   - слово о событии без «если» (commitmentEventWords), если перед ним нет слов о сроке: «как только появятся результаты, позвоним»
//     остаётся обещанием.
var (
	commitmentContingentWords = wordSet("появятся", "появится", "возникнут", "возникнет", "понадобится", "потребуется", "дозвонитесь", "захотите",
		"получится", "случится", "нужно", "надо", "что-то", "вдруг", "захочется")
	commitmentInvitationWords = wordSet("обращайтесь", "обратитесь", "пишите", "напишите", "звоните", "позвоните", "спрашивайте")
	commitmentEventWords      = wordSet("появятся", "возникнут", "понадобится", "потребуется", "вдруг")
	commitmentTimingWords     = wordSet("как", "когда", "после", "пока", "едва")
)

// commitmentOfferWords и commitmentOfferPairs — предложение или условие, а не обещание: «хотите, пришлю», «если нужно, отправим».
// Они учитываются, если стоят не дальше трёх слов перед глаголом в той же фразе. Предложение становится обещанием,
// только когда клиент согласился, и тогда обещание звучит отдельным сообщением.
var (
	commitmentOfferWords = wordSet("хотите", "захотите", "желаете", "давайте")
	commitmentOfferPairs = map[[2]string]struct{}{
		{"если", "хотите"}: {}, {"если", "захотите"}: {}, {"если", "нужно"}: {}, {"если", "надо"}: {}, {"если", "понадобится"}: {},
		{"если", "потребуется"}: {}, {"при", "необходимости"}: {}, {"по", "желанию"}: {}, {"по", "запросу"}: {},
	}
)

var (
	commitmentWords        = wordSet(append(append([]string{}, commitmentFirstPerson...), commitmentThirdPerson...)...)
	commitmentParticipleOf = participleSet()
)

func wordSet(words ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(words))
	for _, word := range words {
		set[word] = struct{}{}
	}
	return set
}

// participleSet разворачивает основы причастий в формы мужского, женского, среднего рода и множественного числа.
func participleSet() map[string]struct{} {
	set := make(map[string]struct{}, len(commitmentParticiples)*4)
	for _, stem := range commitmentParticiples {
		set[stem] = struct{}{}
		for _, ending := range []string{"а", "о", "ы"} {
			set[stem+ending] = struct{}{}
		}
	}
	return set
}

// lexiconToken — слово и номер фразы, в которой оно стоит: фразу заканчивают точка, восклицательный и вопросительный знаки,
// точка с запятой и перевод строки.
type lexiconToken struct {
	word     string
	sentence int
	// clause — номер фразы внутри текста: растёт на каждой запятой, двоеточии, тире и на границе предложения.
	clause int
}

// lexiconTokens приводит текст к виду для сопоставления: нижний регистр, «ё» как «е», единый апостроф;
// слово — последовательность букв, цифр, дефисов и апострофов.
func lexiconTokens(body string) []lexiconToken {
	body = strings.NewReplacer("ё", "е", "Ё", "е", "’", "'", "`", "'").Replace(strings.ToLower(body))
	var tokens []lexiconToken
	var word []rune
	sentence, clause := 0, 0
	flush := func() {
		// Тире, окружённое пробелами, — не слово: иначе оно сдвигало бы окно отрицания и предложения.
		if len(word) > 0 && strings.Trim(string(word), "-'") != "" {
			tokens = append(tokens, lexiconToken{word: string(word), sentence: sentence, clause: clause})
		}
		word = word[:0]
	}
	for _, r := range body {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '\'' || r == '-':
			word = append(word, r)
		default:
			flush()
			if strings.ContainsRune(".!?;…\n", r) {
				sentence++
				clause++
			} else if strings.ContainsRune(",:—–", r) {
				clause++
			}
		}
	}
	flush()
	return tokens
}

// HasCommitmentAnchor сообщает, есть ли в тексте слово будущего действия компании, не стоящее под отрицанием, не
// превращённое в предложение и не смягчённое («возможно», «может быть») в той же фразе. Это допуск к обещанию, а не
// решение: смысл оценивает модель.
func HasCommitmentAnchor(body string) bool {
	tokens := lexiconTokens(body)
	for i := range tokens {
		if !commitmentAnchorAt(tokens, i) || commitmentNegatedAt(tokens, i) || commitmentOfferedAt(tokens, i) || commitmentHedgedAt(tokens, i) || commitmentStandingAt(tokens, i) {
			continue
		}
		return true
	}
	return false
}

// commitmentHedgedAt: в том же предложении, что и слово, до его собственной фразы включительно есть «возможно», «может быть» или
// оборот о привычной практике («обычно», «как правило»). Смягчение действует вперёд: «Возможно, есть скидка. Проверю и напишу»
// оставляет второе предложение обещанием, «Возможно, ответим завтра» и «Обычно перезваниваем в тот же день» — нет. Слово смягчения
// в следующей фразе («Деньги вернём сегодня, обычно банк зачисляет за три дня») относится к ней, а не к слову перед ним, если только
// эта фраза не состоит из одного смягчения: «Ответим завтра, возможно» — вводное слово ко всему предложению.
func commitmentHedgedAt(tokens []lexiconToken, i int) bool {
	for j, token := range tokens {
		if token.sentence != tokens[i].sentence {
			continue
		}
		if token.clause > tokens[i].clause && !parentheticalClause(tokens, j) {
			continue
		}
		if _, hedge := commitmentHedgeWords[token.word]; hedge {
			return true
		}
		if j+1 < len(tokens) && tokens[j+1].sentence == token.sentence {
			if (token.word == "может" && tokens[j+1].word == "быть") || (token.word == "как" && tokens[j+1].word == "правило") {
				return true
			}
		}
	}
	return false
}

// commitmentParentheticalWords — слова вводной фразы смягчения: «возможно», «может быть», «как правило» и связки перед ними.
var commitmentParentheticalWords = wordSet("возможно", "наверное", "вероятно", "пожалуй", "обычно", "может", "быть", "как", "правило", "а", "но", "и")

// parentheticalClause: фраза, в которой стоит токен j, состоит только из слов смягчения и связок.
func parentheticalClause(tokens []lexiconToken, j int) bool {
	for _, other := range tokens {
		if other.sentence != tokens[j].sentence || other.clause != tokens[j].clause {
			continue
		}
		if _, ok := commitmentParentheticalWords[other.word]; !ok {
			return false
		}
	}
	return true
}

// commitmentStandingAt: перед глаголом в той же фразе стоит условие или приглашение, от которого зависит ответ компании.
func commitmentStandingAt(tokens []lexiconToken, i int) bool {
	conditional, timing := false, false
	for j := 0; j < i && tokens[j].sentence <= tokens[i].sentence; j++ {
		if tokens[j].sentence != tokens[i].sentence {
			continue
		}
		word := tokens[j].word
		if _, invited := commitmentInvitationWords[word]; invited {
			return true
		}
		if word == "если" {
			conditional = true
			continue
		}
		if _, hasTiming := commitmentTimingWords[word]; hasTiming {
			timing = true
		}
		if _, contingent := commitmentContingentWords[word]; contingent && conditional {
			return true
		}
		if _, event := commitmentEventWords[word]; event && !timing {
			return true
		}
	}
	return false
}

// commitmentNegatedAt: слово стоит сразу после отрицания.
func commitmentNegatedAt(tokens []lexiconToken, i int) bool {
	if i == 0 || tokens[i-1].sentence != tokens[i].sentence {
		return false
	}
	_, negated := commitmentNegators[tokens[i-1].word]
	return negated
}

// commitmentOfferedAt: не дальше трёх слов перед глаголом в той же фразе стоит предложение или условие.
func commitmentOfferedAt(tokens []lexiconToken, i int) bool {
	for j := i - 1; j >= 0 && i-j <= 3 && tokens[j].sentence == tokens[i].sentence; j-- {
		if _, offered := commitmentOfferWords[tokens[j].word]; offered {
			return true
		}
		if j > 0 && tokens[j-1].sentence == tokens[j].sentence {
			if _, offered := commitmentOfferPairs[[2]string{tokens[j-1].word, tokens[j].word}]; offered {
				return true
			}
		}
	}
	return false
}

// LexicalSupport — насколько словарь поддерживает обещание в исходящем сообщении. Проход по обещаниям (ADR 0055) опирается на
// него как на подсказку: порог уверенности модели выше там, где слов обещания нет вовсе.
type LexicalSupport int

const (
	// LexicalOpen: слов обещания нет, а структура не исключает его (модель одна решает, есть ли обещание).
	LexicalOpen LexicalSupport = iota
	// LexicalAdmitted: есть слово обещания, не исключённое структурой (HasCommitmentAnchor).
	LexicalAdmitted
	// LexicalRuledOut: обещания заведомо нет (CommitmentRuledOut).
	LexicalRuledOut
)

// CommitmentSupport относит тело исходящего сообщения к одному из трёх случаев; допуск слова сильнее исключения.
func CommitmentSupport(body string) LexicalSupport {
	switch {
	case HasCommitmentAnchor(body):
		return LexicalAdmitted
	case CommitmentRuledOut(body):
		return LexicalRuledOut
	}
	return LexicalOpen
}

// CommitmentRuledOut сообщает, что в сообщении заведомо нет обещания компании. Это отсекающая проверка, а не допуск: «нет» не
// означает, что обещание есть. Сообщение исключено, если оно пустое либо каждое его предложение закрыто, а приветствия и благодарности
// ничего не меняют. Предложение закрыто, если
//   - в нём есть слова обещания, но каждое стоит под отрицанием, в предложении, в смягчении или в стоящем предложении
//     («Хотите, пришлю?», «Если нужно, отправим», «Возможно, ответим завтра», «Обращайтесь, отвечу»);
//   - слов обещания нет, а предложение — вопрос, предложение услуги («Могу уточнить у мастера», «Готовы отправить»), предложение
//     или условие («Хотите подборку?») либо смягчённая фраза («Возможно, получится отправить сегодня»).
//
// Слово обещания, не исключённое структурой, и повествовательное предложение без признаков («Будет сделано», «Подготовили документы»)
// оставляют сообщение открытым: исключение отсекает только то, что обещанием быть не может, остальное решает модель.
func CommitmentRuledOut(body string) bool {
	sentences := lexiconSentences(body)
	if len(sentences) == 0 {
		return true
	}
	closed := 0
	for _, sentence := range sentences {
		switch sentenceState(sentence) {
		case sentenceOpen:
			return false
		case sentenceClosed:
			closed++
		}
	}
	return closed > 0
}

type sentenceStateKind int

const (
	sentenceOpen    sentenceStateKind = iota // обещание возможно
	sentenceClosed                           // обещания в предложении быть не может
	sentenceTrivial                          // приветствие или благодарность
)

func sentenceState(sentence lexiconSentence) sentenceStateKind {
	admitted, anchored := sentenceAnchors(sentence)
	switch {
	case admitted:
		return sentenceOpen
	case anchored || sentence.question || closedByWords(sentence):
		return sentenceClosed
	}
	if _, polite := commitmentPoliteWords[sentence.words[0]]; polite && len(sentence.words) <= 3 {
		return sentenceTrivial
	}
	return sentenceOpen
}

// sentenceAnchors сообщает, есть ли в предложении слово обещания, не исключённое структурой (admitted), и есть ли слова обещания
// вообще (anchored).
func sentenceAnchors(sentence lexiconSentence) (admitted, anchored bool) {
	tokens := make([]lexiconToken, len(sentence.words))
	clause, next := 0, 0
	for i, word := range sentence.words {
		for next < len(sentence.breaks) && sentence.breaks[next] <= i {
			clause++
			next++
		}
		tokens[i] = lexiconToken{word: word, clause: clause}
	}
	for i := range tokens {
		if !commitmentAnchorAt(tokens, i) {
			continue
		}
		anchored = true
		if !commitmentNegatedAt(tokens, i) && !commitmentOfferedAt(tokens, i) && !commitmentHedgedAt(tokens, i) && !commitmentStandingAt(tokens, i) {
			return true, true
		}
	}
	return false, anchored
}

// CommitmentStated: слово обещания стоит в повествовательном предложении. Слово в вопросе («Будет ли удобно, если тренер перезвонит?»)
// предлагает, а не обещает.
func CommitmentStated(body string) bool {
	for _, sentence := range lexiconSentences(body) {
		if admitted, _ := sentenceAnchors(sentence); admitted && !sentence.question {
			return true
		}
	}
	return false
}

// CommitmentOpenText оставляет в сообщении только предложения, в которых обещание возможно: закрытые предложения (вопросы, предложения
// услуги, смягчения, исключённые слова обещания) и приветствия убираются. Проход по обещаниям видит именно этот текст, чтобы вопрос
// «Прислать вам фото?» рядом с обычным ответом не принимался за обещание. Если оставлять нечего, возвращается исходный текст.
func CommitmentOpenText(body string) string {
	var kept []string
	for _, sentence := range lexiconSentences(body) {
		if sentenceState(sentence) == sentenceOpen {
			kept = append(kept, sentence.text)
		}
	}
	if len(kept) == 0 {
		return body
	}
	return strings.Join(kept, " ")
}

// closedByWords: предложение без слов обещания закрыто словами-признаками — смягчением, предложением услуги, предложением или условием.
// Предложение услуги от первого лица («могу уточнить», «готовы отправить») закрывает предложение целиком, где бы ни стояло. Остальные признаки
// закрывают предложение, только если стоят в его ведущей фразе (до первой запятой, двоеточия или тире, не считая вежливых слов и
// подтверждений вроде «спасибо», «конечно»): «Возможно, ответим завтра», «Хотите, подберём?». Признак в следующей фразе относится к ней, а
// не к обещанию перед ним: «Деньги вернём сегодня, обычно банк зачисляет за три дня», «В субботу сборщик отрегулирует петли, а если
// понадобится, поставит новые», «Курьер доставит букет, этот можно не возвращать».
func closedByWords(sentence lexiconSentence) bool {
	words := sentence.words
	lead := sentence.leadEnd()
	for i, word := range words {
		if _, speaker := commitmentSpeakerOffers[word]; speaker && hasInfinitiveAfter(words, i, 3) {
			return true
		}
		if i >= lead {
			continue
		}
		if _, hedge := commitmentHedgeWords[word]; hedge {
			return true
		}
		if _, offer := commitmentOfferWords[word]; offer {
			return true
		}
		if i+1 < len(words) {
			next := words[i+1]
			if _, offer := commitmentOfferPairs[[2]string{word, next}]; offer {
				return true
			}
			if (word == "может" && next == "быть") || (word == "как" && next == "правило") {
				return true
			}
		}
		if _, modal := commitmentModalOffers[word]; modal && hasInfinitiveAfter(words, i, 3) {
			return true
		}
	}
	return false
}

// leadEnd — конец ведущей фразы предложения: первая фраза, в которой есть что-то кроме вежливых слов и подтверждений. Если такой нет,
// ведущей считается всё предложение.
func (s lexiconSentence) leadEnd() int {
	start := 0
	for _, end := range append(append([]int{}, s.breaks...), len(s.words)) {
		if end <= start {
			continue
		}
		if !onlyLeadWords(s.words[start:end]) {
			return end
		}
		start = end
	}
	return len(s.words)
}

// onlyLeadWords: во фразе одни вежливые слова и подтверждения («Спасибо», «Да, конечно», «Добрый день»).
func onlyLeadWords(words []string) bool {
	for _, word := range words {
		_, polite := commitmentPoliteWords[word]
		_, confirmed := commitmentConfirmWords[word]
		if !polite && !confirmed {
			return false
		}
	}
	return true
}

// commitmentConfirmWords — подтверждения, которыми начинается ответ: за ними идёт содержательная фраза.
var commitmentConfirmWords = wordSet("да", "конечно", "хорошо", "понятно", "ясно", "отлично", "поняли", "понял", "поняла", "разумеется", "безусловно",
	"ок", "окей", "ладно", "верно", "согласны", "согласен", "согласна")

// hasInfinitiveAfter: в пределах n слов после позиции i есть инфинитив («могу уточнить», «готовы отправить»).
func hasInfinitiveAfter(words []string, i, n int) bool {
	for j := i + 1; j < len(words) && j <= i+n; j++ {
		if isInfinitive(words[j]) {
			return true
		}
	}
	return false
}

// isInfinitive узнаёт русский инфинитив: слово на «-ть» или «-чь» не короче пяти букв либо одно из немногих распространённых слов на «-ти».
// Существительные на «-сти» («новости», «области», «части») инфинитивами не считаются.
func isInfinitive(word string) bool {
	if _, known := commitmentInfinitivesOnTi[word]; known {
		return true
	}
	if len([]rune(word)) < 5 {
		return false // «есть», «мать», «пять», «дать»: слишком коротки, чтобы отличить инфинитив по окончанию
	}
	if _, other := commitmentNotInfinitives[word]; other {
		return false
	}
	return strings.HasSuffix(word, "ть") || strings.HasSuffix(word, "чь")
}

// commitmentNotInfinitives — слова на «-ть», которые инфинитивами не являются.
var commitmentNotInfinitives = wordSet("честь", "весть", "шесть", "девять", "десять", "кровать", "стоимость", "возможность", "необходимость",
	"готовность", "ответственность", "радость", "новость", "сложность", "точность", "срочность", "безопасность", "активность")

// commitmentInfinitivesOnTi — инфинитивы на «-ти», которые встречаются в предложении услуги («могу привезти», «можем найти»).
var commitmentInfinitivesOnTi = wordSet("привезти", "довезти", "отвезти", "завезти", "увезти", "принести", "отнести", "донести", "перевести", "провести",
	"привести", "отвести", "найти", "зайти", "пройти", "дойти", "подойти", "прийти", "выйти")

// commitmentPoliteWords — слова, которыми начинается приветствие или благодарность; такая короткая реплика обещанием не бывает.
var commitmentPoliteWords = wordSet("здравствуйте", "здравствуй", "привет", "приветствую", "добрый", "доброе", "спасибо", "благодарю",
	"пожалуйста", "извините", "простите")

// lexiconSentence — предложение как список слов; question — оно закончено знаком вопроса; text — исходная запись предложения
// (с регистром и знаками) без пробелов по краям.
type lexiconSentence struct {
	words    []string
	question bool
	text     string
	// breaks — индексы слов, с которых начинается каждая следующая фраза предложения (после запятой, двоеточия или тире).
	breaks []int
}

// lexiconSentences разбирает текст теми же правилами, что lexiconTokens, но сохраняет, какие предложения были вопросами, и
// исходную запись каждого предложения.
func lexiconSentences(body string) []lexiconSentence {
	var sentences []lexiconSentence
	var current lexiconSentence
	var word []rune
	var original strings.Builder
	flushWord := func() {
		if len(word) > 0 && strings.Trim(string(word), "-'") != "" {
			current.words = append(current.words, string(word))
		}
		word = word[:0]
	}
	closeSentence := func() {
		flushWord()
		if len(current.words) > 0 {
			current.text = strings.TrimSpace(original.String())
			sentences = append(sentences, current)
		}
		current = lexiconSentence{}
		original.Reset()
	}
	for _, r := range body {
		normalized := unicode.ToLower(r)
		switch normalized {
		case 'ё':
			normalized = 'е'
		case '’', '`':
			normalized = '\''
		}
		switch {
		case unicode.IsLetter(normalized) || unicode.IsDigit(normalized) || normalized == '\'' || normalized == '-':
			word = append(word, normalized)
			original.WriteRune(r)
		case normalized == '?':
			flushWord()
			current.question = true
			original.WriteRune(r)
			closeSentence()
		case strings.ContainsRune(".!;…\n", normalized):
			original.WriteRune(r)
			closeSentence()
		case strings.ContainsRune(",:—–", normalized):
			flushWord()
			if n := len(current.words); n > 0 && (len(current.breaks) == 0 || current.breaks[len(current.breaks)-1] != n) {
				current.breaks = append(current.breaks, n)
			}
			original.WriteRune(r)
		default:
			flushWord()
			original.WriteRune(r)
		}
	}
	closeSentence()
	return sentences
}

func commitmentAnchorAt(tokens []lexiconToken, i int) bool {
	token := tokens[i].word
	if _, ok := commitmentWords[token]; ok {
		return true
	}
	if _, promises := commitmentPromiseVerbs[token]; promises {
		return true
	}
	switch token {
	case "i'll", "we'll":
		return true
	case "will":
		return i+1 < len(tokens)
	case "будет", "будут":
		for _, next := range tokens[i+1 : min(len(tokens), i+3)] {
			if _, ok := commitmentParticipleOf[next.word]; ok {
				return true
			}
		}
		return false
	}
	if _, take := commitmentTakeWords[token]; take {
		for j := i + 1; j+1 < len(tokens) && j <= i+4; j++ {
			if tokens[j].word == "в" && tokens[j+1].word == "работу" {
				return true
			}
		}
	}
	for _, phrase := range commitmentPhrases {
		if len(tokens)-i < len(phrase) || token != phrase[0] {
			continue
		}
		matched := true
		for offset, word := range phrase {
			if tokens[i+offset].word != word {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}
