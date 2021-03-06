// Copyright (c) 2016-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package app

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"

	"github.com/mattermost/mattermost-server/mlog"
	"github.com/mattermost/mattermost-server/model"
	"github.com/mattermost/mattermost-server/utils"
	goi18n "github.com/nicksnyder/go-i18n/i18n"
)

type CommandProvider interface {
	GetTrigger() string
	GetCommand(a *App, T goi18n.TranslateFunc) *model.Command
	DoCommand(a *App, args *model.CommandArgs, message string) *model.CommandResponse
}

var commandProviders = make(map[string]CommandProvider)

func RegisterCommandProvider(newProvider CommandProvider) {
	commandProviders[newProvider.GetTrigger()] = newProvider
}

func GetCommandProvider(name string) CommandProvider {
	provider, ok := commandProviders[name]
	if ok {
		return provider
	}

	return nil
}

func (a *App) CreateCommandPost(post *model.Post, teamId string, response *model.CommandResponse) (*model.Post, *model.AppError) {
	post.Message = model.ParseSlackLinksToMarkdown(response.Text)
	post.CreateAt = model.GetMillis()

	if strings.HasPrefix(post.Type, model.POST_SYSTEM_MESSAGE_PREFIX) {
		err := model.NewAppError("CreateCommandPost", "api.context.invalid_param.app_error", map[string]interface{}{"Name": "post.type"}, "", http.StatusBadRequest)
		return nil, err
	}

	if response.Attachments != nil {
		model.ParseSlackAttachment(post, response.Attachments)
	}

	if response.ResponseType == model.COMMAND_RESPONSE_TYPE_IN_CHANNEL {
		return a.CreatePostMissingChannel(post, true)
	}

	if (response.ResponseType == "" || response.ResponseType == model.COMMAND_RESPONSE_TYPE_EPHEMERAL) && (response.Text != "" || response.Attachments != nil) {
		post.ParentId = ""
		a.SendEphemeralPost(post.UserId, post)
	}

	return post, nil
}

// previous ListCommands now ListAutocompleteCommands
func (a *App) ListAutocompleteCommands(teamId string, T goi18n.TranslateFunc) ([]*model.Command, *model.AppError) {
	commands := make([]*model.Command, 0, 32)
	seen := make(map[string]bool)
	for _, value := range commandProviders {
		if cmd := value.GetCommand(a, T); cmd != nil {
			cpy := *cmd
			if cpy.AutoComplete && !seen[cpy.Id] {
				cpy.Sanitize()
				seen[cpy.Trigger] = true
				commands = append(commands, &cpy)
			}
		}
	}

	for _, cmd := range a.PluginCommandsForTeam(teamId) {
		if cmd.AutoComplete && !seen[cmd.Trigger] {
			seen[cmd.Trigger] = true
			commands = append(commands, cmd)
		}
	}

	if *a.Config().ServiceSettings.EnableCommands {
		result := <-a.Srv.Store.Command().GetByTeam(teamId)
		if result.Err != nil {
			return nil, result.Err
		}

		teamCmds := result.Data.([]*model.Command)
		for _, cmd := range teamCmds {
			if cmd.AutoComplete && !seen[cmd.Id] {
				cmd.Sanitize()
				seen[cmd.Trigger] = true
				commands = append(commands, cmd)
			}
		}
	}

	return commands, nil
}

func (a *App) ListTeamCommands(teamId string) ([]*model.Command, *model.AppError) {
	if !*a.Config().ServiceSettings.EnableCommands {
		return nil, model.NewAppError("ListTeamCommands", "api.command.disabled.app_error", nil, "", http.StatusNotImplemented)
	}

	result := <-a.Srv.Store.Command().GetByTeam(teamId)
	if result.Err != nil {
		return nil, result.Err
	}

	return result.Data.([]*model.Command), nil
}

func (a *App) ListAllCommands(teamId string, T goi18n.TranslateFunc) ([]*model.Command, *model.AppError) {
	commands := make([]*model.Command, 0, 32)
	seen := make(map[string]bool)
	for _, value := range commandProviders {
		if cmd := value.GetCommand(a, T); cmd != nil {
			cpy := *cmd
			if cpy.AutoComplete && !seen[cpy.Trigger] {
				cpy.Sanitize()
				seen[cpy.Trigger] = true
				commands = append(commands, &cpy)
			}
		}
	}

	for _, cmd := range a.PluginCommandsForTeam(teamId) {
		if !seen[cmd.Trigger] {
			seen[cmd.Trigger] = true
			commands = append(commands, cmd)
		}
	}

	if *a.Config().ServiceSettings.EnableCommands {
		result := <-a.Srv.Store.Command().GetByTeam(teamId)
		if result.Err != nil {
			return nil, result.Err
		}
		teamCmds := result.Data.([]*model.Command)
		for _, cmd := range teamCmds {
			if !seen[cmd.Trigger] {
				cmd.Sanitize()
				seen[cmd.Trigger] = true
				commands = append(commands, cmd)
			}
		}
	}

	return commands, nil
}

func (a *App) ExecuteCommand(args *model.CommandArgs) (*model.CommandResponse, *model.AppError) {
	parts := strings.Split(args.Command, " ")
	trigger := parts[0][1:]
	trigger = strings.ToLower(trigger)
	message := strings.Join(parts[1:], " ")
	provider := GetCommandProvider(trigger)

	clientTriggerId, triggerId, appErr := model.GenerateTriggerId(args.UserId, a.AsymmetricSigningKey())
	if appErr != nil {
		mlog.Error(appErr.Error())
	}

	args.TriggerId = triggerId

	if provider != nil {
		if cmd := provider.GetCommand(a, args.T); cmd != nil {
			response := provider.DoCommand(a, args, message)
			return a.HandleCommandResponse(cmd, args, response, true)
		}
	}

	cmd, response, appErr := a.ExecutePluginCommand(args)
	if appErr != nil {
		return nil, appErr
	}
	if cmd != nil {
		response.TriggerId = clientTriggerId
		return a.HandleCommandResponse(cmd, args, response, true)
	}

	if !*a.Config().ServiceSettings.EnableCommands {
		return nil, model.NewAppError("ExecuteCommand", "api.command.disabled.app_error", nil, "", http.StatusNotImplemented)
	}

	chanChan := a.Srv.Store.Channel().Get(args.ChannelId, true)
	teamChan := a.Srv.Store.Team().Get(args.TeamId)
	userChan := a.Srv.Store.User().Get(args.UserId)

	result := <-a.Srv.Store.Command().GetByTeam(args.TeamId)
	if result.Err != nil {
		return nil, result.Err
	}

	tr := <-teamChan
	if tr.Err != nil {
		return nil, tr.Err
	}
	team := tr.Data.(*model.Team)

	ur := <-userChan
	if ur.Err != nil {
		return nil, ur.Err
	}
	user := ur.Data.(*model.User)

	cr := <-chanChan
	if cr.Err != nil {
		return nil, cr.Err
	}
	channel := cr.Data.(*model.Channel)

	teamCmds := result.Data.([]*model.Command)
	for _, cmd := range teamCmds {
		if trigger == cmd.Trigger {
			mlog.Debug(fmt.Sprintf(utils.T("api.command.execute_command.debug"), trigger, args.UserId))

			p := url.Values{}
			p.Set("token", cmd.Token)

			p.Set("team_id", cmd.TeamId)
			p.Set("team_domain", team.Name)

			p.Set("channel_id", args.ChannelId)
			p.Set("channel_name", channel.Name)

			p.Set("user_id", args.UserId)
			p.Set("user_name", user.Username)

			p.Set("command", "/"+trigger)
			p.Set("text", message)

			p.Set("trigger_id", triggerId)

			hook, appErr := a.CreateCommandWebhook(cmd.Id, args)
			if appErr != nil {
				return nil, model.NewAppError("command", "api.command.execute_command.failed.app_error", map[string]interface{}{"Trigger": trigger}, appErr.Error(), http.StatusInternalServerError)
			}
			p.Set("response_url", args.SiteURL+"/hooks/commands/"+hook.Id)

			var req *http.Request
			if cmd.Method == model.COMMAND_METHOD_GET {
				req, _ = http.NewRequest(http.MethodGet, cmd.URL, nil)

				if req.URL.RawQuery != "" {
					req.URL.RawQuery += "&"
				}
				req.URL.RawQuery += p.Encode()
			} else {
				req, _ = http.NewRequest(http.MethodPost, cmd.URL, strings.NewReader(p.Encode()))
			}

			req.Header.Set("Accept", "application/json")
			req.Header.Set("Authorization", "Token "+cmd.Token)
			if cmd.Method == model.COMMAND_METHOD_POST {
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			}

			resp, err := a.HTTPService.MakeClient(false).Do(req)
			if err != nil {
				return nil, model.NewAppError("command", "api.command.execute_command.failed.app_error", map[string]interface{}{"Trigger": trigger}, err.Error(), http.StatusInternalServerError)
			}
			if resp.StatusCode != http.StatusOK {
				defer resp.Body.Close()
				body, _ := ioutil.ReadAll(resp.Body)
				return nil, model.NewAppError("command", "api.command.execute_command.failed_resp.app_error", map[string]interface{}{"Trigger": trigger, "Status": resp.Status}, string(body), http.StatusInternalServerError)
			}

			response, err := model.CommandResponseFromHTTPBody(resp.Header.Get("Content-Type"), resp.Body)
			if err != nil {
				return nil, model.NewAppError("command", "api.command.execute_command.failed.app_error", map[string]interface{}{"Trigger": trigger}, err.Error(), http.StatusInternalServerError)
			}
			if response == nil {
				return nil, model.NewAppError("command", "api.command.execute_command.failed_empty.app_error", map[string]interface{}{"Trigger": trigger}, "", http.StatusInternalServerError)
			}

			response.TriggerId = clientTriggerId

			return a.HandleCommandResponse(cmd, args, response, false)
		}
	}

	return nil, model.NewAppError("command", "api.command.execute_command.not_found.app_error", map[string]interface{}{"Trigger": trigger}, "", http.StatusNotFound)
}

func (a *App) HandleCommandResponse(command *model.Command, args *model.CommandArgs, response *model.CommandResponse, builtIn bool) (*model.CommandResponse, *model.AppError) {
	a.HandleCommandResponsePost(command, args, response, builtIn)

	if response.ExtraResponses != nil {
		for _, resp := range response.ExtraResponses {
			a.HandleCommandResponsePost(command, args, resp, builtIn)
		}
	}

	return response, nil
}

func (a *App) HandleCommandResponsePost(command *model.Command, args *model.CommandArgs, response *model.CommandResponse, builtIn bool) (*model.CommandResponse, *model.AppError) {
	post := &model.Post{}
	post.ChannelId = args.ChannelId
	post.RootId = args.RootId
	post.ParentId = args.ParentId
	post.UserId = args.UserId
	post.Type = response.Type
	post.Props = response.Props

	isBotPost := !builtIn

	if a.Config().ServiceSettings.EnablePostUsernameOverride {
		if len(command.Username) != 0 {
			post.AddProp("override_username", command.Username)
			isBotPost = true
		} else if len(response.Username) != 0 {
			post.AddProp("override_username", response.Username)
			isBotPost = true
		}
	}

	if a.Config().ServiceSettings.EnablePostIconOverride {
		if len(command.IconURL) != 0 {
			post.AddProp("override_icon_url", command.IconURL)
			isBotPost = true
		} else if len(response.IconURL) != 0 {
			post.AddProp("override_icon_url", response.IconURL)
			isBotPost = true
		} else {
			post.AddProp("override_icon_url", "")
		}
	}

	if isBotPost {
		post.AddProp("from_webhook", "true")
	}

	// Process Slack text replacements
	response.Text = a.ProcessSlackText(response.Text)
	response.Attachments = a.ProcessSlackAttachments(response.Attachments)

	if _, err := a.CreateCommandPost(post, args.TeamId, response); err != nil {
		mlog.Error(err.Error())
	}

	return response, nil
}

func (a *App) CreateCommand(cmd *model.Command) (*model.Command, *model.AppError) {
	if !*a.Config().ServiceSettings.EnableCommands {
		return nil, model.NewAppError("CreateCommand", "api.command.disabled.app_error", nil, "", http.StatusNotImplemented)
	}

	cmd.Trigger = strings.ToLower(cmd.Trigger)

	result := <-a.Srv.Store.Command().GetByTeam(cmd.TeamId)
	if result.Err != nil {
		return nil, result.Err
	}

	teamCmds := result.Data.([]*model.Command)
	for _, existingCommand := range teamCmds {
		if cmd.Trigger == existingCommand.Trigger {
			return nil, model.NewAppError("CreateCommand", "api.command.duplicate_trigger.app_error", nil, "", http.StatusBadRequest)
		}
	}

	for _, builtInProvider := range commandProviders {
		builtInCommand := builtInProvider.GetCommand(a, utils.T)
		if builtInCommand != nil && cmd.Trigger == builtInCommand.Trigger {
			return nil, model.NewAppError("CreateCommand", "api.command.duplicate_trigger.app_error", nil, "", http.StatusBadRequest)
		}
	}

	result = <-a.Srv.Store.Command().Save(cmd)
	if result.Err != nil {
		return nil, result.Err
	}

	return result.Data.(*model.Command), nil
}

func (a *App) GetCommand(commandId string) (*model.Command, *model.AppError) {
	if !*a.Config().ServiceSettings.EnableCommands {
		return nil, model.NewAppError("GetCommand", "api.command.disabled.app_error", nil, "", http.StatusNotImplemented)
	}

	result := <-a.Srv.Store.Command().Get(commandId)
	if result.Err != nil {
		result.Err.StatusCode = http.StatusNotFound
		return nil, result.Err
	}

	return result.Data.(*model.Command), nil
}

func (a *App) UpdateCommand(oldCmd, updatedCmd *model.Command) (*model.Command, *model.AppError) {
	if !*a.Config().ServiceSettings.EnableCommands {
		return nil, model.NewAppError("UpdateCommand", "api.command.disabled.app_error", nil, "", http.StatusNotImplemented)
	}

	updatedCmd.Trigger = strings.ToLower(updatedCmd.Trigger)
	updatedCmd.Id = oldCmd.Id
	updatedCmd.Token = oldCmd.Token
	updatedCmd.CreateAt = oldCmd.CreateAt
	updatedCmd.UpdateAt = model.GetMillis()
	updatedCmd.DeleteAt = oldCmd.DeleteAt
	updatedCmd.CreatorId = oldCmd.CreatorId
	updatedCmd.TeamId = oldCmd.TeamId

	result := <-a.Srv.Store.Command().Update(updatedCmd)
	if result.Err != nil {
		return nil, result.Err
	}
	return result.Data.(*model.Command), nil
}

func (a *App) MoveCommand(team *model.Team, command *model.Command) *model.AppError {
	command.TeamId = team.Id

	result := <-a.Srv.Store.Command().Update(command)
	if result.Err != nil {
		return result.Err
	}

	return nil
}

func (a *App) RegenCommandToken(cmd *model.Command) (*model.Command, *model.AppError) {
	if !*a.Config().ServiceSettings.EnableCommands {
		return nil, model.NewAppError("RegenCommandToken", "api.command.disabled.app_error", nil, "", http.StatusNotImplemented)
	}

	cmd.Token = model.NewId()

	result := <-a.Srv.Store.Command().Update(cmd)
	if result.Err != nil {
		return nil, result.Err
	}

	return result.Data.(*model.Command), nil
}

func (a *App) DeleteCommand(commandId string) *model.AppError {
	if !*a.Config().ServiceSettings.EnableCommands {
		return model.NewAppError("DeleteCommand", "api.command.disabled.app_error", nil, "", http.StatusNotImplemented)
	}
	result := <-a.Srv.Store.Command().Delete(commandId, model.GetMillis())
	if result.Err != nil {
		return result.Err
	}
	return nil
}
