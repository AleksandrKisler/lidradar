-- Только через dev-data: внешняя транзакция, проверка среды/базы, журнал и
-- временный frontend_profiles принадлежат исполнителю. Файл не входит в
-- рабочие миграции PostgreSQL. Все имена, переписки и суммы синтетические.
DO $$
DECLARE
    p RECORD;
    i INTEGER; j INTEGER; k INTEGER; scenario INTEGER; point INTEGER;
    location_ids UUID[]; channel_ids UUID[]; service_ids UUID[];
    contact UUID; conversation UUID; opportunity UUID; message_ids UUID[];
    risk UUID; action UUID; outcome UUID; revenue UUID; notification UUID;
    item UUID; mid UUID; at TIMESTAMPTZ := transaction_timestamp();
    first_at TIMESTAMPTZ; last_at TIMESTAMPTZ; sent_at TIMESTAMPTZ;
    due TIMESTAMPTZ; closed_at_value TIMESTAMPTZ;
    stage TEXT; previous_stage TEXT; steps TEXT[];
    risk_type TEXT; risk_status TEXT; severity_value TEXT;
    explanation TEXT; advice TEXT; direction_value TEXT; body TEXT;
    texts TEXT[]; amount NUMERIC(14,2);
    type_names TEXT[] := ARRAY['NO_RESPONSE','BOOKING_NOT_CONFIRMED','PROMISE_NOT_FULFILLED','CUSTOMER_SILENT_AFTER_PRICE','FOLLOW_UP_CANDIDATE'];
    client_names TEXT[] := ARRAY['Анна Смирнова','Михаил Волков','Софья Белова','Даниил Орлов','Мария Лебедева','Артём Соколов','Елена Морозова','Илья Кузнецов','Виктория Соловьёва','Никита Петров','Александра Константинопольская-Рождественская','Ли Мэй'];
BEGIN
    -- Коллизия — ошибка, а не обновление чужого аккаунта или его пароля.
    IF EXISTS (SELECT 1 FROM users u JOIN frontend_profiles reserved ON u.id=reserved.user_id OR u.email=reserved.email)
       OR EXISTS (SELECT 1 FROM organizations o JOIN frontend_profiles reserved ON o.id=reserved.tenant_id) THEN
        RAISE EXCEPTION 'зарезервированные реквизиты уже заняты' USING ERRCODE='23505';
    END IF;
    FOR p IN SELECT * FROM frontend_profiles ORDER BY number LOOP
        INSERT INTO users(id,email,password_hash,display_name,status,created_at,updated_at)
        VALUES(p.user_id,p.email,p.password_hash,'Разработчик · '||p.name,'ACTIVE',at-INTERVAL '29 days',at);
        INSERT INTO organizations(id,name,default_timezone,default_currency,status,created_at,updated_at)
        VALUES(p.tenant_id,p.name,'Europe/Moscow','RUB','ACTIVE',at-INTERVAL '29 days',at);
        INSERT INTO memberships(id,tenant_id,user_id,role,status,created_at,updated_at)
        VALUES(uuidv7(),p.tenant_id,p.user_id,'OWNER','ACTIVE',at-INTERVAL '29 days',at);
        -- Пустой кабинет действительно пуст: нет даже точек и каталога.
        IF p.conversation_count=0 THEN CONTINUE; END IF;
        location_ids := ARRAY[]::UUID[]; channel_ids := ARRAY[]::UUID[]; service_ids := ARRAY[]::UUID[];
        FOR j IN 1..p.location_count LOOP
            item := uuidv7(); location_ids := array_append(location_ids,item);
            INSERT INTO locations(id,tenant_id,name,timezone,response_threshold_minutes,created_at,updated_at)
            VALUES(item,p.tenant_id,(ARRAY['На Цветном бульваре','На Арбате','На Петроградской'])[j],
                   'Europe/Moscow',45,at-INTERVAL '29 days',at);
            FOR k IN 1..7 LOOP
                INSERT INTO location_business_hours(id,tenant_id,location_id,weekday,is_closed,opens_at,closes_at,created_at,updated_at)
                VALUES(uuidv7(),p.tenant_id,item,k,FALSE,'09:00','21:00',at-INTERVAL '29 days',at);
            END LOOP;
            FOR k IN 1..4 LOOP
                mid := uuidv7();
                IF k=1 THEN service_ids := array_append(service_ids,mid); END IF;
                INSERT INTO service_catalog_items(id,tenant_id,location_id,name,normalized_name,price_from,price_to,currency,active,created_at,updated_at)
                VALUES(mid,p.tenant_id,item,
                    (ARRAY['Стрижка и укладка','Окрашивание','Консультация стилиста','Вечерняя укладка'])[k],
                    (ARRAY['стрижка и укладка','окрашивание','консультация стилиста','вечерняя укладка'])[k],
                    (ARRAY[3500,6000,NULL,4500]::NUMERIC[])[k],
                    (ARRAY[3500,12000,NULL,4500]::NUMERIC[])[k],'RUB',k<>4,at-INTERVAL '29 days',at);
            END LOOP;
            mid := uuidv7(); channel_ids := array_append(channel_ids,mid);
            INSERT INTO channel_connections(id,tenant_id,location_id,provider,name,status,capabilities,verification_secret_hash,last_event_at,last_success_at,last_error_at,last_error_code,created_at,updated_at)
            VALUES(mid,p.tenant_id,item,'TEST','Учебный канал №'||j||' · без Telegram',
                (ARRAY['ACTIVE','DEGRADED','ERROR'])[j],
                '["CAN_RECEIVE_MESSAGES","CAN_RECEIVE_EDITS","CAN_RECEIVE_DELETES","CAN_RECEIVE_ATTACHMENTS","CAN_IDENTIFY_CONTACT"]',
                encode(sha256(uuidv7()::TEXT::BYTEA),'hex'),at-INTERVAL '1 day',at-INTERVAL '2 days',
                CASE WHEN j>1 THEN at-INTERVAL '1 day' END,CASE WHEN j>1 THEN 'SYNTHETIC_CHANNEL_UNAVAILABLE' END,
                at-INTERVAL '29 days',at);
        END LOOP;
        IF p.location_count>1 THEN
            INSERT INTO channel_connections(id,tenant_id,location_id,provider,name,status,capabilities,verification_secret_hash,created_at,updated_at)
            VALUES(uuidv7(),p.tenant_id,location_ids[1],'IMPORT','Архивный импорт · учебный','DISCONNECTED',
                '["CAN_RECEIVE_MESSAGES"]',encode(sha256(uuidv7()::TEXT::BYTEA),'hex'),at-INTERVAL '29 days',at);
        END IF;
        FOR k IN 1..5 LOOP
            INSERT INTO notification_preferences(id,tenant_id,user_id,risk_type,minimum_severity,delivery_mode,in_app_enabled,telegram_enabled,quiet_hours_enabled,quiet_hours_start,quiet_hours_end,digest_time,created_at,updated_at)
            VALUES(uuidv7(),p.tenant_id,p.user_id,type_names[k],'LOW',CASE WHEN k<4 THEN 'IMMEDIATE' ELSE 'DIGEST' END,
                TRUE,FALSE,p.number=3,'22:00','08:00','09:00',at-INTERVAL '29 days',at);
        END LOOP;
        FOR i IN 0..p.conversation_count-1 LOOP
            scenario := i%12; point := 1+(i/12)%p.location_count;
            contact := uuidv7(); conversation := uuidv7(); opportunity := uuidv7();
            -- Есть свежие случаи и история за последние 24 дня. Последняя
            -- оплата всегда в прошлом, даже при запуске миграции ночью.
            first_at := at - make_interval(days=>i/12) - INTERVAL '8 hours'
                - CASE WHEN scenario IN (3,4,6,9) THEN INTERVAL '4 days' ELSE INTERVAL '0 days' END;
            due := first_at + CASE WHEN scenario IN (3,4,6,9) THEN INTERVAL '4 days' ELSE INTERVAL '2 hours' END;
            last_at := CASE WHEN scenario BETWEEN 5 AND 9 THEN due+INTERVAL '1 hour' ELSE first_at+INTERVAL '50 minutes' END;
            stage := (ARRAY['NEW','BOOKING_INTENT','WAITING_BUSINESS','PRICE_SENT','WAITING_CUSTOMER','WON','WON','BOOKED','LOST','ARCHIVED','QUALIFYING','ENGAGED'])[scenario+1];
            amount := CASE WHEN scenario=10 THEN NULL ELSE 3500 END;
            closed_at_value := CASE WHEN stage IN ('WON','LOST','ARCHIVED') THEN due+INTERVAL '3 hours' END;
            texts := ARRAY['Здравствуйте! Подскажите, пожалуйста, по стрижке и укладке.',
                'Здравствуйте! Стрижка и укладка стоят 3500 ₽. Какое время вам удобно?',
                'Мне удобнее во второй половине дня.',
                'Проверю расписание и подберу время.',
                'Спасибо, буду ждать.',
                (ARRAY['Есть ли свободное время завтра в 16:00?',
                    'Запишите меня, пожалуйста, на завтра в 16:00.',
                    'Пришлю варианты сегодня до 18:00.',
                    'Итоговая стоимость — 3500 ₽. Напишите, если нужно подобрать время.',
                    'Мне нужно подумать. Можете напомнить через несколько дней?',
                    'Спасибо за звонок! Запись подтверждаю, оплатила 3500 ₽.',
                    'Спасибо, оплатила 3500 ₽, увидимся завтра!',
                    'Мы уже договорились по телефону, запись подтверждена.',
                    'Спасибо, больше не актуально — выбрала другое время.',
                    'Пока отложим. Если понадобится, напишу сама.',
                    'Пока не определилась, сначала хочу консультацию.',
                    'Длинная строка и символы для проверки интерфейса: <script>alert("учебный текст")</script> & "кавычки" — это текст, не разметка. 🙂'])[scenario+1]];
            INSERT INTO contacts(id,tenant_id,display_name,email_normalized,created_at,updated_at)
            VALUES(contact,p.tenant_id,client_names[scenario+1]||' · '||(i+1),
                CASE WHEN scenario%3<>0 THEN 'client-'||p.number||'-'||i||'@example.test' END,first_at,last_at);
            INSERT INTO external_identities(id,tenant_id,contact_id,provider,connection_id,external_id,metadata,created_at)
            VALUES(uuidv7(),p.tenant_id,contact,'TEST',channel_ids[point],'frontend-contact-'||i,'{"synthetic":true}',first_at);
            INSERT INTO conversations(id,tenant_id,location_id,connection_id,contact_id,external_id,status,first_message_at,last_message_at,last_message_direction,revision,created_at,updated_at)
            VALUES(conversation,p.tenant_id,location_ids[point],channel_ids[point],contact,'frontend-conversation-'||i,
                CASE WHEN scenario=9 THEN 'ARCHIVED' ELSE 'ACTIVE' END,first_at,last_at,
                CASE WHEN scenario IN (2,3) THEN 'OUTGOING' ELSE 'INCOMING' END,
                CASE WHEN scenario=11 THEN 7 ELSE 6 END,first_at,last_at);
            message_ids := ARRAY[]::UUID[];
            FOR j IN 1..6 LOOP
                mid := uuidv7(); message_ids := array_append(message_ids,mid);
                sent_at := first_at + make_interval(mins=>(j-1)*10);
                IF scenario BETWEEN 5 AND 9 AND j>=5 THEN sent_at := due+make_interval(mins=>(j-4)*30); END IF;
                direction_value := CASE WHEN j IN (2,4) OR (j=6 AND scenario IN (2,3)) THEN 'OUTGOING' ELSE 'INCOMING' END;
                body := texts[j];
                INSERT INTO messages(id,tenant_id,conversation_id,connection_id,external_id,direction,type,text,sender_external_id,reply_to_message_id,sent_at,received_at,provider_deleted_at,metadata,created_at)
                VALUES(mid,p.tenant_id,conversation,channel_ids[point],'frontend-message-'||i||'-'||j,direction_value,
                    CASE WHEN scenario=10 AND j=3 THEN 'IMAGE' ELSE 'TEXT' END,
                    CASE WHEN scenario=10 AND j=3 THEN NULL ELSE body END,'frontend-contact-'||i,
                    CASE WHEN j=6 THEN message_ids[2] END,sent_at,sent_at,
                    CASE WHEN scenario=11 AND j=3 THEN sent_at+INTERVAL '1 minute' END,
                    '{"synthetic":true}',sent_at);
                IF scenario=10 AND j=3 THEN
                    INSERT INTO attachments(id,tenant_id,message_id,object_key,mime_type,size_bytes,created_at)
                    VALUES(uuidv7(),p.tenant_id,mid,'fixtures/not-uploaded/'||mid||'.jpg','image/jpeg',245760,sent_at);
                END IF;
            END LOOP;
            INSERT INTO opportunities(id,tenant_id,conversation_id,service_id,stage,estimated_amount,estimated_amount_confidence,currency,opened_at,closed_at,created_at,updated_at)
            VALUES(opportunity,p.tenant_id,conversation,CASE WHEN scenario<>10 THEN service_ids[point] END,stage,amount,
                CASE WHEN amount IS NOT NULL THEN 1 END,'RUB',first_at,closed_at_value,first_at,
                COALESCE(closed_at_value,last_at));
            -- История воспроизводит допустимый путь: WON только после BOOKED,
            -- ARCHIVED — после LOST. Начальная запись всегда NULL → NEW.
            steps := CASE WHEN stage='WON' THEN ARRAY['NEW','BOOKED','WON']
                          WHEN stage='ARCHIVED' THEN ARRAY['NEW','LOST','ARCHIVED']
                          WHEN stage='NEW' THEN ARRAY['NEW'] ELSE ARRAY['NEW',stage] END;
            previous_stage := NULL;
            FOR j IN 1..array_length(steps,1) LOOP
                INSERT INTO opportunity_stage_history(id,tenant_id,opportunity_id,from_stage,to_stage,source,created_at)
                VALUES(uuidv7(),p.tenant_id,opportunity,previous_stage,steps[j],'IMPORT',
                    CASE WHEN j=1 THEN first_at WHEN steps[j] IN ('BOOKED','WON','LOST','ARCHIVED') THEN due+make_interval(mins=>150+j) ELSE last_at END);
                previous_stage := steps[j];
            END LOOP;
            IF scenario>=10 THEN CONTINUE; END IF;
            risk := uuidv7();
            risk_type := type_names[1+scenario%5];
            IF scenario=6 THEN risk_type := 'CUSTOMER_SILENT_AFTER_PRICE'; END IF;
            IF scenario=7 THEN risk_type := 'BOOKING_NOT_CONFIRMED'; END IF;
            IF scenario=8 THEN risk_type := 'NO_RESPONSE'; END IF;
            severity_value := CASE WHEN risk_type='BOOKING_NOT_CONFIRMED' OR scenario=0 THEN 'CRITICAL'
                                   WHEN risk_type IN ('NO_RESPONSE','PROMISE_NOT_FULFILLED') THEN 'HIGH' ELSE 'MEDIUM' END;
            risk_status := (ARRAY['OPEN','ACKNOWLEDGED','OPEN','OPEN','ACTED','RESOLVED','RESOLVED','FALSE_POSITIVE','IGNORED','EXPIRED'])[scenario+1];
            explanation := (ARRAY['Клиент ждёт ответа на вопрос о свободном времени.',
                'Клиент хочет записаться, но подтверждение не отправлено.',
                'Обещанные варианты времени не отправлены к названному сроку.',
                'После сообщения с ценой клиент не вернулся к разговору.',
                'Клиент отложил решение и разрешил напомнить о себе.'])[array_position(type_names,risk_type)];
            advice := CASE risk_type
                WHEN 'NO_RESPONSE' THEN 'Ответить клиенту и предложить следующий шаг.'
                WHEN 'BOOKING_NOT_CONFIRMED' THEN 'Предложить клиенту конкретное свободное время.'
                WHEN 'PROMISE_NOT_FULFILLED' THEN 'Выполнить обещанное или сообщить новый точный срок.'
                WHEN 'CUSTOMER_SILENT_AFTER_PRICE' THEN 'Напомнить о предложении и уточнить, остались ли вопросы.'
                ELSE 'Уточнить, остаётся ли услуга актуальной.' END;
            INSERT INTO risk_signals(id,tenant_id,opportunity_id,location_id,type,severity,status,reason_code,reason_text,source,risk_engine_version,trigger_message_id,detected_at,due_at,acknowledged_at,acted_at,resolved_at,created_at,updated_at)
            VALUES(risk,p.tenant_id,opportunity,location_ids[point],risk_type,severity_value,risk_status,
                'SYNTHETIC_FRONTEND_CASE','Учебный пример: '||explanation,'MANUAL','frontend-fixture/v1',
                message_ids[CASE WHEN scenario BETWEEN 5 AND 9 THEN 3 ELSE 6 END],due,due,
                CASE WHEN risk_status IN ('ACKNOWLEDGED','ACTED','RESOLVED') THEN due+INTERVAL '10 minutes' END,
                CASE WHEN risk_status IN ('ACTED','RESOLVED') THEN due+INTERVAL '20 minutes' END,
                CASE WHEN scenario BETWEEN 5 AND 9 THEN due+INTERVAL '2 hours' END,due,
                CASE WHEN scenario BETWEEN 5 AND 9 THEN due+INTERVAL '2 hours' ELSE due+INTERVAL '20 minutes' END);
            INSERT INTO recommendations(id,tenant_id,risk_id,text,source,created_at)
            VALUES(uuidv7(),p.tenant_id,risk,advice,'TEMPLATE',due);
            action := NULL; outcome := NULL;
            IF risk_status IN ('ACTED','RESOLVED') THEN
                action := uuidv7();
                INSERT INTO actions(id,tenant_id,risk_id,opportunity_id,actor_user_id,type,note,created_at)
                VALUES(action,p.tenant_id,risk,opportunity,p.user_id,'CALL','Учебный пример: связались с клиентом.',due+INTERVAL '20 minutes');
                INSERT INTO audit_log(id,tenant_id,actor_user_id,operation,entity_type,entity_id,created_at)
                VALUES(uuidv7(),p.tenant_id,p.user_id,'ACTION_RECORDED','ACTION',action,due+INTERVAL '20 minutes');
            END IF;
            IF scenario BETWEEN 4 AND 9 THEN
                outcome := uuidv7();
                INSERT INTO outcomes(id,tenant_id,opportunity_id,actor_user_id,status,note,created_at)
                VALUES(outcome,p.tenant_id,opportunity,p.user_id,
                    CASE WHEN scenario IN (5,6) THEN 'PAID' WHEN scenario=7 THEN 'BOOKED' WHEN scenario=8 THEN 'LOST' ELSE 'THINKING' END,
                    'Учебный исход, не результат обработки реального клиента.',due+INTERVAL '2 hours');
                INSERT INTO audit_log(id,tenant_id,actor_user_id,operation,entity_type,entity_id,created_at)
                VALUES(uuidv7(),p.tenant_id,p.user_id,'OUTCOME_RECORDED','OUTCOME',outcome,due+INTERVAL '2 hours');
            END IF;
            IF scenario IN (5,6) THEN
                revenue := uuidv7();
                INSERT INTO revenue_events(id,tenant_id,opportunity_id,amount,currency,status,source,confirmed_by_user_id,confirmed_at,created_at)
                VALUES(revenue,p.tenant_id,opportunity,3500,'RUB','CONFIRMED','USER_CONFIRMED',p.user_id,due+INTERVAL '3 hours',due+INTERVAL '3 hours');
                INSERT INTO revenue_attributions(id,tenant_id,revenue_event_id,opportunity_id,type,risk_id,action_id,outcome_id,created_at)
                VALUES(uuidv7(),p.tenant_id,revenue,opportunity,CASE WHEN scenario=5 THEN 'RECOVERED' ELSE 'ORGANIC' END,
                    CASE WHEN scenario=5 THEN risk END,CASE WHEN scenario=5 THEN action END,CASE WHEN scenario=5 THEN outcome END,due+INTERVAL '3 hours');
                INSERT INTO audit_log(id,tenant_id,actor_user_id,operation,entity_type,entity_id,created_at)
                VALUES(uuidv7(),p.tenant_id,p.user_id,'REVENUE_CONFIRMED','REVENUE_EVENT',revenue,due+INTERVAL '3 hours');
            END IF;
            IF scenario IN (5,6,7) THEN
                INSERT INTO risk_feedback(id,tenant_id,risk_id,opportunity_id,actor_user_id,verdict,reason,note,risk_type,severity,risk_status,source,risk_engine_version,trigger_message_id,opportunity_stage,detected_at,dataset_eligible,created_at)
                VALUES(uuidv7(),p.tenant_id,risk,opportunity,p.user_id,CASE WHEN scenario=7 THEN 'FALSE_POSITIVE' ELSE 'TRUE_POSITIVE' END,
                    CASE WHEN scenario=7 THEN 'CUSTOMER_ALREADY_BOOKED' END,'Учебный вердикт.',risk_type,severity_value,
                    CASE WHEN scenario=7 THEN 'OPEN' ELSE risk_status END,'MANUAL','frontend-fixture/v1',message_ids[3],stage,due,FALSE,due+INTERVAL '3 hours');
            END IF;
            notification := uuidv7();
            INSERT INTO notifications(id,tenant_id,user_id,risk_id,kind,dedup_key,title,body,created_at,updated_at)
            VALUES(notification,p.tenant_id,p.user_id,risk,'RISK_OPENED','risk:'||risk||':opened:user:'||p.user_id,
                'Учебный сигнал · '||client_names[scenario+1],explanation,due,due);
            INSERT INTO notification_deliveries(id,tenant_id,notification_id,channel,destination,title,body,attempt,status,available_at,attempted_at,provider_message_id,kind,created_at,updated_at)
            VALUES(uuidv7(),p.tenant_id,notification,'IN_APP',p.user_id::TEXT,'Учебный сигнал',explanation,1,'SUCCEEDED',due,due,
                'synthetic:'||notification,'RISK_OPENED',due,due);
        END LOOP;
    END LOOP;
END $$;
