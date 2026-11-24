package api

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/log"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/srl-labs/clab-api-server/internal/models"
)

// NTG-516: the node's VNC display listens on its management address only.
const (
	nodeVncPort        = 5900
	nodeVncDialTimeout = 5 * time.Second
)

// StreamNodeVncHandler pipes a websocket to the node's VNC display (RFB over binary frames, as noVNC expects).
// @Summary Stream a node's VNC display
// @Description Opens a websocket carrying the raw RFB stream of the node's display on its management address, port 5900.
// @Tags Nodes
// @Security BearerAuth
// @Param labName path string true "Name of the lab" example="my-lab"
// @Param nodeName path string true "Name of the node" example="pc1"
// @Success 101 {string} string "Switching protocols"
// @Failure 400 {object} models.ErrorResponse "Invalid lab or node name"
// @Failure 404 {object} models.ErrorResponse "Lab or node not found"
// @Failure 409 {object} models.ErrorResponse "Node not running or has no management IPv4"
// @Failure 502 {object} models.ErrorResponse "The node's display is not reachable"
// @Router /api/v1/labs/{labName}/nodes/{nodeName}/vnc [get]
func StreamNodeVncHandler(c *gin.Context) {
	username := c.GetString("username")
	labName := strings.TrimSpace(c.Param("labName"))
	nodeName := strings.TrimSpace(c.Param("nodeName"))

	if !isValidLabName(labName) {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Invalid lab name format."})
		return
	}
	if nodeName == "" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Error: "Node name is required."})
		return
	}
	if _, err := verifyLabOwnership(c, username, labName); err != nil {
		return
	}
	svc := GetClabService()
	if svc == nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Error: "Containerlab service not initialized"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	container, err := resolveLabNodeContainer(ctx, svc, labName, nodeName)
	if err != nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse{Error: err.Error()})
		return
	}
	ip := strings.TrimSpace(container.NetworkSettings.IPv4addr)
	if !strings.EqualFold(container.State, "running") || ip == "" {
		c.JSON(http.StatusConflict, models.ErrorResponse{Error: "Node is not running or has no management IPv4 address."})
		return
	}

	// Dial before the upgrade so a missing display is an HTTP error the caller can show.
	upstream, err := net.DialTimeout("tcp", net.JoinHostPort(ip, strconv.Itoa(nodeVncPort)), nodeVncDialTimeout)
	if err != nil {
		log.Infof("vnc: user '%s' lab '%s' node '%s': display not reachable: %v", username, labName, nodeName, err)
		c.JSON(http.StatusBadGateway, models.ErrorResponse{Error: "The node's display is not reachable."})
		return
	}
	defer upstream.Close()

	conn, err := terminalUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Warnf("vnc websocket upgrade failed for user '%s' lab '%s' node '%s': %v", username, labName, nodeName, err)
		return
	}
	defer conn.Close()
	log.Infof("vnc: user '%s' opened lab '%s' node '%s'", username, labName, nodeName)
	pipeVnc(conn, upstream)
	log.Infof("vnc: user '%s' closed lab '%s' node '%s'", username, labName, nodeName)
}

// pipeVnc copies bytes both ways until either side ends, then closes both.
func pipeVnc(ws *websocket.Conn, upstream net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			kind, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if kind != websocket.BinaryMessage && kind != websocket.TextMessage {
				continue
			}
			if _, err := upstream.Write(data); err != nil {
				return
			}
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 64*1024)
		for {
			n, err := upstream.Read(buf)
			if n > 0 {
				if werr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
					log.Debugf("vnc: upstream read ended: %v", err)
				}
				return
			}
		}
	}()
	<-done
	_ = upstream.Close()
	_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	_ = ws.Close()
	<-done
}
